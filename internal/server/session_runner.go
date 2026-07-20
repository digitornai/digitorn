package server

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/digitornai/digitorn/internal/runtime"
)

var errTurnAborted = errors.New("turn aborted by client")

type sessionRunner struct {
	exec   func(ctx context.Context, in runtime.TurnInput) error
	cutoff time.Duration
	logger *slog.Logger

	states sync.Map

	cancelMu sync.Mutex
	inflight map[string]context.CancelCauseFunc
}

type sessionRunState struct {
	mu      sync.Mutex
	running bool
	pending bool
	next    runtime.TurnInput
	lastJWT string
}

func newSessionRunner(
	exec func(ctx context.Context, in runtime.TurnInput) error,
	cutoff time.Duration,
	logger *slog.Logger,
) *sessionRunner {
	if cutoff <= 0 {
		cutoff = turnSafetyCutoff
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &sessionRunner{exec: exec, cutoff: cutoff, logger: logger, inflight: map[string]context.CancelCauseFunc{}}
}

func (r *sessionRunner) Abort(sessionID string) bool {
	if r == nil {
		return false
	}
	if v, ok := r.states.Load(sessionID); ok {
		st := v.(*sessionRunState)
		st.mu.Lock()
		st.pending = false
		st.next = runtime.TurnInput{}
		st.mu.Unlock()
	}
	r.cancelMu.Lock()
	cancel := r.inflight[sessionID]
	r.cancelMu.Unlock()
	if cancel == nil {
		return false
	}
	cancel(errTurnAborted)
	return true
}

// IsRunning reports whether a turn is currently executing for the session (a
// cancel func is registered for the whole runOnce lifetime). Used to re-arm the
// client's spinner on join/reconnect when a turn is in flight.
func (r *sessionRunner) IsRunning(sessionID string) bool {
	if r == nil {
		return false
	}
	r.cancelMu.Lock()
	_, ok := r.inflight[sessionID]
	r.cancelMu.Unlock()
	return ok
}

func (r *sessionRunner) registerCancel(sessionID string, cancel context.CancelCauseFunc) {
	r.cancelMu.Lock()
	r.inflight[sessionID] = cancel
	r.cancelMu.Unlock()
}

func (r *sessionRunner) clearCancel(sessionID string) {
	r.cancelMu.Lock()
	delete(r.inflight, sessionID)
	r.cancelMu.Unlock()
}

func (r *sessionRunner) WakeTurn(in runtime.TurnInput) {
	if r == nil || in.SessionID == "" || in.AppID == "" {
		return
	}
	v, _ := r.states.LoadOrStore(in.SessionID, &sessionRunState{})
	st := v.(*sessionRunState)

	st.mu.Lock()
	if in.UserJWT != "" {
		st.lastJWT = in.UserJWT
	} else if st.lastJWT != "" {
		in.UserJWT = st.lastJWT
	}
	if st.running {
		st.pending = true
		st.next = in
		st.mu.Unlock()
		return
	}
	st.running = true
	st.mu.Unlock()

	go r.loop(st, in)
}

func (r *sessionRunner) WakeSession(appID, sessionID, userID string) {
	r.WakeTurn(runtime.TurnInput{AppID: appID, SessionID: sessionID, UserID: userID})
}

func (r *sessionRunner) Forget(sessionID string) {
	if r == nil {
		return
	}
	v, ok := r.states.Load(sessionID)
	if !ok {
		return
	}
	st := v.(*sessionRunState)
	st.mu.Lock()
	running := st.running
	st.mu.Unlock()
	if !running {
		r.states.Delete(sessionID)
	}
}

func (r *sessionRunner) loop(st *sessionRunState, in runtime.TurnInput) {
	for {
		r.runOnce(in)

		st.mu.Lock()
		if st.pending {
			st.pending = false
			in = st.next
			st.mu.Unlock()
			continue
		}
		st.running = false
		st.mu.Unlock()
		return
	}
}

func (r *sessionRunner) runOnce(in runtime.TurnInput) {
	defer func() {
		if rec := recover(); rec != nil {
			r.logger.Error("session_runner: turn panicked",
				slog.String("session_id", in.SessionID),
				slog.String("app_id", in.AppID),
				slog.Any("panic", rec))
		}
	}()
	base, abort := context.WithCancelCause(context.Background())
	defer abort(nil)
	watchdog := time.AfterFunc(r.cutoff, func() { abort(runtime.ErrTurnSafetyCutoff) })
	defer watchdog.Stop()
	ctx := runtime.WithTurnKeepalive(base, func() { watchdog.Reset(r.cutoff) })
	r.registerCancel(in.SessionID, abort)
	defer r.clearCancel(in.SessionID)
	if err := r.exec(ctx, in); err != nil {
		r.logger.Error("session_runner: turn failed",
			slog.String("session_id", in.SessionID),
			slog.String("app_id", in.AppID),
			slog.String("err", err.Error()))
	}
}
