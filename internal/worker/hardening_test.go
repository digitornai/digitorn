package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/health/grpc_health_v1"
)

// H5 — Worker framework hardening. The existing manager_test.go
// covers the basic happy paths ; this file targets the edge cases
// that have been the source of real bugs : clean-exit restart, port
// re-bind after restart, goroutine leak across cycles, MaxFailures
// circuit, multi-Kind isolation, slow startup, env propagation,
// concurrent spawn.

// TestHardening_CleanExitTriggersRestart is a regression test for the
// bug where a worker exiting with status 0 was treated as graceful
// shutdown and NOT restarted. After the fix, only an explicit
// Stop()/ctx.Cancel() counts as graceful — anything else (exit 0,
// exit 1, SIGKILL) is a crash and must restart.
func TestHardening_CleanExitTriggersRestart(t *testing.T) {
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
	// DUMMY_EXIT_AFTER_MS causes the worker to call os.Exit(0) cleanly
	// after 600ms — exactly the case the bug missed.
	if err := m.Spawn(ctx, Spec{
		Kind:        "dummy",
		Binary:      exe,
		Count:       1,
		Env:         map[string]string{"DUMMY_EXIT_AFTER_MS": "600"},
		BackoffMin:  100 * time.Millisecond,
		BackoffMax:  500 * time.Millisecond,
		MaxFailures: 10,
	}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pool := m.Pool("dummy")
		if len(pool) > 0 && pool[0].Restarts >= 2 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	pool := m.Pool("dummy")
	if len(pool) == 0 {
		t.Fatal("worker missing from pool")
	}
	t.Fatalf("clean-exit worker did not restart : got %d restarts", pool[0].Restarts)
}

// TestHardening_PortRebindAfterRestart is a regression test for the
// managedConn caching bug. After a worker crashes and re-spawns on a
// new ephemeral port, Pick() must return a connection to the NEW
// address — not a stale one to the dead port.
func TestHardening_PortRebindAfterRestart(t *testing.T) {
	exe := buildDummyWorker(t)
	m := NewManager(quietLogger())
	_ = m.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, Spec{
		Kind: "dummy", Binary: exe, Count: 1,
		StartTimeout: 10 * time.Second,
		BackoffMin:   100 * time.Millisecond,
		BackoffMax:   500 * time.Millisecond,
		MaxFailures:  10,
	}); err != nil {
		t.Fatal(err)
	}

	// Warm a connection via the first instance.
	c1, err := m.Pick(ctx, "dummy")
	if err != nil {
		t.Fatalf("pick #1: %v", err)
	}
	if _, err := HealthCheck(ctx, c1, ""); err != nil {
		t.Fatalf("health #1: %v", err)
	}
	pool := m.Pool("dummy")
	firstPID := pool[0].PID
	firstAddr := pool[0].Address

	// Kill the worker process directly to force a restart on a new
	// port (ephemeral allocation = address changes).
	p, err := os.FindProcess(firstPID)
	if err != nil {
		t.Fatalf("FindProcess: %v", err)
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	// Wait for the supervisor to restart.
	deadline := time.Now().Add(10 * time.Second)
	var newAddr string
	for time.Now().Before(deadline) {
		pool := m.Pool("dummy")
		if len(pool) > 0 && pool[0].Restarts >= 1 && pool[0].Address != "" && pool[0].PID != firstPID {
			newAddr = pool[0].Address
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if newAddr == "" {
		t.Fatal("worker did not restart within 10s")
	}
	if newAddr == firstAddr {
		t.Logf("note : new address %s matches old %s — that's fine if the OS reused the port", newAddr, firstAddr)
	}

	// A new Pick() MUST return a healthy connection.
	pickCtx, pickCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer pickCancel()
	c2, err := m.Pick(pickCtx, "dummy")
	if err != nil {
		t.Fatalf("pick #2 after restart: %v", err)
	}
	st, err := HealthCheck(pickCtx, c2, "")
	if err != nil {
		t.Fatalf("health #2 after restart: %v (cached stale connection?)", err)
	}
	if st != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("health #2 status: %s", st)
	}
}

// TestHardening_MaxFailuresCircuit verifies that a worker repeatedly
// crashing past MaxFailures stops being respawned. Otherwise a broken
// binary causes a tight restart loop that burns CPU.
func TestHardening_MaxFailuresCircuit(t *testing.T) {
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
	// Worker always crashes immediately (exit 99).
	err := m.Spawn(ctx, Spec{
		Kind: "crash-loop", Binary: exe, Count: 1,
		Env:          map[string]string{"DUMMY_CRASH_ON_START": "1"},
		StartTimeout: 500 * time.Millisecond,
		BackoffMin:   50 * time.Millisecond,
		BackoffMax:   100 * time.Millisecond,
		MaxFailures:  3,
	})
	// Initial Spawn likely returns an error (worker never reached Ready).
	if err == nil {
		t.Log("Spawn returned no error — supervisor may give the worker a chance after the initial deadline")
	}

	// Give the supervisor time to exhaust MaxFailures.
	time.Sleep(2 * time.Second)

	pool := m.Pool("crash-loop")
	if len(pool) == 0 {
		// Acceptable : pool fully reaped after circuit opened.
		return
	}
	if pool[0].Restarts > 10 {
		t.Errorf("MaxFailures=3 was ignored : %d restarts observed", pool[0].Restarts)
	}
	if pool[0].Status == StatusReady || pool[0].Status == StatusRunning {
		t.Errorf("worker should not be running after exhausting MaxFailures, status=%s", pool[0].Status)
	}
}

// TestHardening_NoGoroutineLeak_50SpawnStopCycles spawns and stops a
// manager 50 times in sequence and verifies the goroutine count doesn't
// grow unbounded. The framework owns supervisors, gRPC conns and a
// background reaper ; any of those leaking is a long-running daemon
// killer.
func TestHardening_NoGoroutineLeak_50SpawnStopCycles(t *testing.T) {
	exe := buildDummyWorker(t)

	runtime.GC()
	baseline := runtime.NumGoroutine()
	t.Logf("baseline goroutines : %d", baseline)

	for i := 0; i < 50; i++ {
		m := NewManager(quietLogger())
		_ = m.Start()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := m.Spawn(ctx, Spec{Kind: "dummy", Binary: exe, Count: 1}); err != nil {
			cancel()
			t.Fatalf("cycle %d spawn: %v", i, err)
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := m.Stop(stopCtx); err != nil {
			stopCancel()
			cancel()
			t.Fatalf("cycle %d stop: %v", i, err)
		}
		stopCancel()
		cancel()
	}

	// Give some time for goroutines from the last cycle to settle.
	time.Sleep(300 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()
	t.Logf("after 50 spawn/stop cycles : %d goroutines (delta %d)", after, after-baseline)
	// Allow modest growth ; tight assertion catches catastrophic leaks.
	if after > baseline+15 {
		t.Errorf("goroutine leak suspected : baseline=%d after=%d delta=%d",
			baseline, after, after-baseline)
	}
}

// TestHardening_SlowStartupExceedsTimeout proves that a worker which
// blocks past StartTimeout produces a clear error and is killed, not
// left in limbo.
func TestHardening_SlowStartupExceedsTimeout(t *testing.T) {
	exe := buildDummyWorker(t)
	m := NewManager(quietLogger())
	_ = m.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := m.Spawn(ctx, Spec{
		Kind:         "slow",
		Binary:       exe,
		Count:        1,
		Env:          map[string]string{"DUMMY_STARTUP_DELAY_MS": "5000"}, // 5s delay
		StartTimeout: 500 * time.Millisecond,                              // we wait only 500ms
		BackoffMin:   50 * time.Millisecond,
		BackoffMax:   100 * time.Millisecond,
		MaxFailures:  1,
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error for slow worker")
	}
	if elapsed > 3*time.Second {
		t.Errorf("Spawn waited too long despite StartTimeout=500ms : %v", elapsed)
	}
}

// TestHardening_MultiKindIsolation spawns workers of two different
// kinds and verifies they don't cross-contaminate (pool/picker/stats
// are partitioned by Kind).
func TestHardening_MultiKindIsolation(t *testing.T) {
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
	if err := m.Spawn(ctx, Spec{Kind: "alpha", Binary: exe, Count: 2}); err != nil {
		t.Fatal(err)
	}
	if err := m.Spawn(ctx, Spec{Kind: "beta", Binary: exe, Count: 3}); err != nil {
		t.Fatal(err)
	}

	pa := m.Pool("alpha")
	pb := m.Pool("beta")
	if len(pa) != 2 {
		t.Fatalf("alpha pool : %d", len(pa))
	}
	if len(pb) != 3 {
		t.Fatalf("beta pool : %d", len(pb))
	}

	// Pick from alpha 20× → only alpha IDs.
	alphaIDs := map[string]bool{}
	for _, h := range pa {
		alphaIDs[h.ID] = true
	}
	for i := 0; i < 20; i++ {
		c, err := m.Pick(ctx, "alpha")
		if err != nil {
			t.Fatalf("pick alpha %d: %v", i, err)
		}
		if !alphaIDs[c.Handle().ID] {
			t.Fatalf("pick(alpha) returned a foreign-kind handle : %s", c.Handle().ID)
		}
	}

	// Stats partitioned.
	st := m.Stats()
	if st.Pools["alpha"].Total != 2 || st.Pools["beta"].Total != 3 {
		t.Fatalf("stats partitioning : alpha=%+v beta=%+v",
			st.Pools["alpha"], st.Pools["beta"])
	}
}

// TestHardening_EnvPropagation verifies that Spec.Env reaches the
// worker subprocess. We can't read the worker's env directly, but
// we can use it to control the dummy's behavior (DUMMY_CRASH_ON_START)
// and observe the consequence.
func TestHardening_EnvPropagation(t *testing.T) {
	exe := buildDummyWorker(t)
	m := NewManager(quietLogger())
	_ = m.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := m.Spawn(ctx, Spec{
		Kind:         "envtest",
		Binary:       exe,
		Count:        1,
		Env:          map[string]string{"DUMMY_CRASH_ON_START": "1"},
		StartTimeout: 800 * time.Millisecond,
		BackoffMin:   50 * time.Millisecond,
		BackoffMax:   100 * time.Millisecond,
		MaxFailures:  2,
	})
	// If the env reached the worker, it crashed before reaching Ready.
	if err == nil {
		t.Fatal("worker did not crash despite DUMMY_CRASH_ON_START=1 ; env did not propagate")
	}
	if !errors.Is(err, ErrStartupTimeout) && !errors.Is(err, ErrSpawnFailed) {
		t.Logf("got error class : %v (acceptable as long as Spawn failed)", err)
	}
}

// TestHardening_ConcurrentSpawn fires many Spawn calls in parallel on
// the same manager. Each call uses a unique Kind ; the manager must
// serialise its internal state changes without deadlocking or losing
// any spawned pool.
func TestHardening_ConcurrentSpawn(t *testing.T) {
	exe := buildDummyWorker(t)
	m := NewManager(quietLogger())
	_ = m.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	}()

	const N = 8
	var wg sync.WaitGroup
	var errs atomic.Int64
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			err := m.Spawn(ctx, Spec{
				Kind: Kind(fmt.Sprintf("kind-%d", i)), Binary: exe, Count: 1,
			})
			if err != nil {
				errs.Add(1)
				t.Errorf("spawn kind-%d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	if errs.Load() != 0 {
		t.Fatalf("%d concurrent spawns errored", errs.Load())
	}

	// Each Kind must have exactly 1 worker, addressable by name.
	for i := 0; i < N; i++ {
		kind := fmt.Sprintf("kind-%d", i)
		pool := m.Pool(Kind(kind))
		if len(pool) != 1 {
			t.Errorf("%s : pool size = %d", kind, len(pool))
		}
	}
}

// TestHardening_PickRespectsContextDeadline ensures that Pick() doesn't
// hang forever waiting for a healthy worker — if ctx expires, it must
// return promptly with a clear error.
func TestHardening_PickRespectsContextDeadline(t *testing.T) {
	m := NewManager(quietLogger())
	_ = m.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	}()

	// No workers spawned — Pick must time out via ctx.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := m.Pick(ctx, "phantom-kind")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected Pick to error without workers")
	}
	if elapsed > 1*time.Second {
		t.Errorf("Pick blocked past ctx deadline : %v", elapsed)
	}
}
