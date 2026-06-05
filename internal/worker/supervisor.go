package worker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// supervisor manages the full lifecycle of a single worker subprocess.
// It owns : the *exec.Cmd, the captured stdout/stderr ring, the
// restart logic, the discovered gRPC address. One supervisor per instance.
type supervisor struct {
	id      string
	spec    Spec
	envSec  string // secret pushed to the child via env
	logger  *slog.Logger
	onReady func(addr string)

	status   atomic.Int32 // Status
	restarts atomic.Int32
	address  atomic.Pointer[string]
	pid      atomic.Int32
	startAt  atomic.Pointer[time.Time]

	cmdMu      sync.Mutex
	cmd        *exec.Cmd
	stdoutRing *ringBuffer
	stderrRing *ringBuffer

	stopCh  chan struct{}
	doneCh  chan struct{}
	started atomic.Bool
}

func newSupervisor(id string, spec Spec, logger *slog.Logger) *supervisor {
	if logger == nil {
		logger = slog.Default()
	}
	return &supervisor{
		id:         id,
		spec:       spec,
		envSec:     generateSecret(),
		logger:     logger,
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
		stdoutRing: newRingBuffer(spec.StdoutRing),
		stderrRing: newRingBuffer(spec.StdoutRing),
	}
}

func (s *supervisor) Start(ctx context.Context) error {
	if !s.started.CompareAndSwap(false, true) {
		return nil
	}
	s.status.Store(int32(StatusStarting))
	// Launch one spawn-and-watch goroutine that handles restarts.
	go s.runLoop(ctx)
	return s.waitReady(ctx)
}

// waitReady blocks until the worker writes its ready line OR start times out.
func (s *supervisor) waitReady(ctx context.Context) error {
	d := s.spec.StartTimeout
	deadline := time.Now().Add(d)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if a := s.address.Load(); a != nil && *a != "" {
			s.status.Store(int32(StatusReady))
			return nil
		}
		if s.status.Load() == int32(StatusFailed) {
			return ErrSpawnFailed
		}
		if time.Now().After(deadline) {
			s.logger.Error("worker: startup timeout",
				slog.String("id", s.id),
				slog.String("stdout", s.stdoutRing.snapshot()),
				slog.String("stderr", s.stderrRing.snapshot()))
			return ErrStartupTimeout
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return ctx.Err()
		case <-s.stopCh:
			return ErrManagerStopped
		}
	}
}

// runLoop repeatedly spawns the subprocess, supervises it, restarts on
// crash with exponential backoff, and stops on shutdown signal.
func (s *supervisor) runLoop(parentCtx context.Context) {
	defer close(s.doneCh)

	backoff := s.spec.BackoffMin
	consecutiveFails := 0

	for {
		select {
		case <-s.stopCh:
			return
		case <-parentCtx.Done():
			return
		default:
		}

		err := s.spawnAndWait(parentCtx)
		// Only treat the exit as "clean" if we explicitly asked to Stop
		// (or the parent context was cancelled). Any other exit — even
		// status 0 — is a crash and we must restart.
		select {
		case <-s.stopCh:
			s.status.Store(int32(StatusStopped))
			return
		default:
		}
		if errors.Is(err, context.Canceled) {
			s.status.Store(int32(StatusStopped))
			return
		}
		errStr := "clean exit"
		if err != nil {
			errStr = err.Error()
		}
		s.logger.Warn("worker: instance exited, will restart",
			slog.String("id", s.id),
			slog.String("err", errStr),
			slog.Int("backoff_ms", int(backoff/time.Millisecond)))
		consecutiveFails++
		s.restarts.Add(1)
		if s.spec.MaxFailures > 0 && consecutiveFails >= s.spec.MaxFailures {
			s.status.Store(int32(StatusFailed))
			s.logger.Error("worker: max failures exceeded, giving up",
				slog.String("id", s.id),
				slog.Int("failures", consecutiveFails))
			return
		}

		select {
		case <-time.After(backoff):
		case <-s.stopCh:
			return
		case <-parentCtx.Done():
			return
		}
		// Exponential backoff up to BackoffMax.
		backoff *= 2
		if backoff > s.spec.BackoffMax {
			backoff = s.spec.BackoffMax
		}

		// Reset consecutive failure counter if the previous instance ran
		// long enough to be considered healthy.
		if s.runDuration() > 30*time.Second {
			consecutiveFails = 0
			backoff = s.spec.BackoffMin
		}

		// Clear the previous address — a new instance will publish a new one.
		empty := ""
		s.address.Store(&empty)
	}
}

func (s *supervisor) spawnAndWait(ctx context.Context) error {
	cmd := exec.Command(s.spec.Binary, s.spec.Args...)
	cmd.Env = s.buildEnv()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	configureProcAttr(cmd)

	s.cmdMu.Lock()
	s.cmd = cmd
	s.cmdMu.Unlock()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	// Bind the worker's lifetime to the daemon's : on Windows it joins the
	// kill-on-close Job Object so it dies with the daemon even on a hard crash
	// (no more orphaned digitorn-worker-* processes). No-op on unix, where the
	// process group already covers graceful shutdown.
	trackChild(cmd)
	s.pid.Store(int32(cmd.Process.Pid))
	now := time.Now()
	s.startAt.Store(&now)
	s.status.Store(int32(StatusRunning))

	go s.pumpLines(stdout, s.stdoutRing, true)
	go s.pumpLines(stderr, s.stderrRing, false)

	// Reap. cmd.Wait returns when the process exits.
	exitErr := cmd.Wait()
	if exitErr == nil {
		return nil
	}
	return exitErr
}

func (s *supervisor) pumpLines(r io.Reader, ring *ringBuffer, isStdout bool) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		ring.append(line)
		if isStdout && strings.HasPrefix(line, readyLinePrefix) {
			addr := strings.TrimPrefix(line, readyLinePrefix)
			addr = strings.TrimSpace(addr)
			s.address.Store(&addr)
			if s.onReady != nil {
				s.onReady(addr)
			}
		}
	}
}

func (s *supervisor) buildEnv() []string {
	env := os.Environ()
	env = append(env,
		EnvSecretKey+"="+s.envSec,
		"DIGITORN_WORKER_KIND="+string(s.spec.Kind),
		"DIGITORN_WORKER_ID="+s.id,
	)
	for k, v := range s.spec.Env {
		env = append(env, k+"="+v)
	}
	return env
}

func (s *supervisor) runDuration() time.Duration {
	t := s.startAt.Load()
	if t == nil {
		return 0
	}
	return time.Since(*t)
}

// Stop signals the supervisor to drain and stop. Blocks until the
// runLoop exits OR ctx expires.
func (s *supervisor) Stop(ctx context.Context) error {
	if !s.started.Load() {
		return nil
	}
	s.status.Store(int32(StatusDraining))
	// Tell runLoop to exit.
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
	// Send SIGTERM to the child if alive.
	s.cmdMu.Lock()
	cmd := s.cmd
	s.cmdMu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = sendStopSignal(cmd.Process)
		// Schedule a hard kill if the graceful stop times out.
		go func() {
			deadline := time.Now().Add(s.spec.StopTimeout)
			tick := time.NewTicker(50 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-s.doneCh:
					return
				case <-tick.C:
					if time.Now().After(deadline) {
						s.logger.Warn("worker: hard kill after stop timeout",
							slog.String("id", s.id))
						_ = hardKill(cmd.Process)
						return
					}
				}
			}
		}()
	}
	select {
	case <-s.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *supervisor) snapshot() Handle {
	addr := ""
	if a := s.address.Load(); a != nil {
		addr = *a
	}
	startAt := time.Time{}
	if t := s.startAt.Load(); t != nil {
		startAt = *t
	}
	return Handle{
		ID:       s.id,
		Kind:     s.spec.Kind,
		PID:      int(s.pid.Load()),
		Address:  addr,
		Status:   Status(s.status.Load()),
		Restarts: int(s.restarts.Load()),
		StartAt:  startAt,
	}
}

func (s *supervisor) Stdout() string { return s.stdoutRing.snapshot() }
func (s *supervisor) Stderr() string { return s.stderrRing.snapshot() }
