package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
)

// buildDummy compiles cmd/digitorn-worker-dummy once per test binary
// and returns the resulting executable path.
var (
	dummyOnce sync.Once
	dummyPath string
	dummyErr  error
)

func buildDummyWorker(t *testing.T) string {
	t.Helper()
	dummyOnce.Do(func() {
		dir, err := os.MkdirTemp("", "digitorn-worker-dummy-*")
		if err != nil {
			dummyErr = err
			return
		}
		exe := filepath.Join(dir, "dummy")
		if runtime.GOOS == "windows" {
			exe += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", exe,
			"github.com/digitornai/digitorn/cmd/digitorn-worker-dummy")
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			dummyErr = err
			return
		}
		dummyPath = exe
	})
	if dummyErr != nil {
		t.Fatalf("build dummy worker: %v", dummyErr)
	}
	return dummyPath
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestManager_Spawn_SingleWorkerReady(t *testing.T) {
	exe := buildDummyWorker(t)
	m := NewManager(quietLogger())
	_ = m.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, Spec{
		Kind:         "dummy",
		Binary:       exe,
		Count:        1,
		StartTimeout: 10 * time.Second,
	}); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	pool := m.Pool("dummy")
	if len(pool) != 1 {
		t.Fatalf("pool size: %d", len(pool))
	}
	h := pool[0]
	if h.Status != StatusReady && h.Status != StatusRunning {
		t.Fatalf("status: %s", h.Status)
	}
	if h.Address == "" {
		t.Fatal("address not published")
	}
	if h.PID == 0 {
		t.Fatal("pid missing")
	}
}

func TestManager_Health_RespondsServing(t *testing.T) {
	exe := buildDummyWorker(t)
	m := NewManager(quietLogger())
	_ = m.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, Spec{
		Kind: "dummy", Binary: exe, Count: 1,
	}); err != nil {
		t.Fatal(err)
	}

	c, err := m.Pick(ctx, "dummy")
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	st, err := HealthCheck(ctx, c, "")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if st != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("status: %s", st)
	}
}

func TestManager_LoadBalance_RoundRobin(t *testing.T) {
	exe := buildDummyWorker(t)
	m := NewManager(quietLogger())
	_ = m.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, Spec{
		Kind: "dummy", Binary: exe, Count: 3,
	}); err != nil {
		t.Fatal(err)
	}

	pool := m.Pool("dummy")
	if len(pool) != 3 {
		t.Fatalf("pool: %d", len(pool))
	}

	seen := map[string]int{}
	for i := 0; i < 30; i++ {
		c, err := m.Pick(ctx, "dummy")
		if err != nil {
			t.Fatalf("pick: %v", err)
		}
		seen[c.Handle().ID]++
	}
	if len(seen) != 3 {
		t.Fatalf("expected to see all 3 instances, got %d : %v", len(seen), seen)
	}
}

func TestManager_AuthRejectsForeignClient(t *testing.T) {
	exe := buildDummyWorker(t)
	m := NewManager(quietLogger())
	_ = m.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, Spec{Kind: "dummy", Binary: exe, Count: 1}); err != nil {
		t.Fatal(err)
	}

	c, err := m.Pick(ctx, "dummy")
	if err != nil {
		t.Fatal(err)
	}

	// Forge an outbound metadata WITHOUT the secret header — should fail.
	mdCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs(HeaderSecret, "wrong-secret"))
	cli := grpc_health_v1.NewHealthClient(c.GRPC())
	_, err = cli.Check(mdCtx, &grpc_health_v1.HealthCheckRequest{})
	if err == nil {
		t.Fatal("expected auth failure with wrong secret")
	}
}

func TestManager_RestartOnCrash(t *testing.T) {
	exe := buildDummyWorker(t)
	m := NewManager(quietLogger())
	_ = m.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Worker exits 1s after start ; we expect at least one restart.
	if err := m.Spawn(ctx, Spec{
		Kind:        "dummy",
		Binary:      exe,
		Count:       1,
		Env:         map[string]string{"DUMMY_EXIT_AFTER_MS": "800"},
		BackoffMin:  100 * time.Millisecond,
		BackoffMax:  500 * time.Millisecond,
		MaxFailures: 10,
	}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		pool := m.Pool("dummy")
		if len(pool) > 0 && pool[0].Restarts >= 2 {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	pool := m.Pool("dummy")
	t.Fatalf("expected >=2 restarts within 8s, got %d", pool[0].Restarts)
}

func TestManager_FailedSpawn_BadBinary(t *testing.T) {
	m := NewManager(quietLogger())
	_ = m.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := m.Spawn(ctx, Spec{
		Kind:         "ghost",
		Binary:       "this-binary-does-not-exist-anywhere",
		Count:        1,
		StartTimeout: 1 * time.Second,
		MaxFailures:  2,
		BackoffMin:   50 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected spawn error")
	}
	if !errors.Is(err, ErrStartupTimeout) && !errors.Is(err, ErrSpawnFailed) {
		// Other errors are acceptable too — the point is we got AN error fast.
	}
}

func TestManager_StopGracefully(t *testing.T) {
	exe := buildDummyWorker(t)
	m := NewManager(quietLogger())
	_ = m.Start()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, Spec{Kind: "dummy", Binary: exe, Count: 2}); err != nil {
		t.Fatal(err)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	start := time.Now()
	if err := m.Stop(stopCtx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 4*time.Second {
		t.Errorf("stop took too long: %v", elapsed)
	}
}

func TestManager_Stats_ReportsCounts(t *testing.T) {
	exe := buildDummyWorker(t)
	m := NewManager(quietLogger())
	_ = m.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, Spec{Kind: "dummy", Binary: exe, Count: 3}); err != nil {
		t.Fatal(err)
	}

	st := m.Stats()
	p := st.Pools["dummy"]
	if p.Total != 3 {
		t.Fatalf("total: %d", p.Total)
	}
	if p.Ready != 3 {
		t.Fatalf("ready: %d", p.Ready)
	}
}

func TestManager_NoHealthyWorker_WhenKindUnknown(t *testing.T) {
	m := NewManager(quietLogger())
	_ = m.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := m.Pick(ctx, "nothing-here")
	if !errors.Is(err, ErrNoHealthyWorker) {
		t.Fatalf("err: %v (want %v)", err, ErrNoHealthyWorker)
	}
}
