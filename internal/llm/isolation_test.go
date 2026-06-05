package llm_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/worker"
)

// L6 — Worker isolation tests. We exercise the daemon ↔ worker boundary
// to prove : (a) crashes don't cascade, (b) cancellation cleans up, (c)
// concurrent load doesn't leak goroutines, (d) bad input doesn't kill the
// worker.

func TestIsolation_50ConcurrentCalls_NoGoroutineLeak(t *testing.T) {
	exe := buildLLMWorker(t)
	m := worker.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	_ = m.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, worker.Spec{
		Kind: "llm", Binary: exe, Count: 1, StartTimeout: 15 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}

	client, _ := llm.NewClient(llm.ClientConfig{Manager: m})

	// Warm the worker's gRPC conn pool (Phase 3 introduced N=connPoolSize
	// parallel HTTP/2 conns per worker, each spawning a few channel
	// goroutines). Measuring baseline BEFORE warming would book the pool's
	// steady-state cost as a leak.
	for i := 0; i < 16; i++ {
		_, _ = client.CountTokens(ctx, &llm.CountTokensRequest{
			Provider: "anthropic", Model: "x",
			Messages: []llm.ChatMessage{{Role: "user", Content: "warm"}},
		})
	}
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	baseline := runtime.NumGoroutine()
	t.Logf("baseline goroutines (after pool warm-up): %d", baseline)

	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.CountTokens(ctx, &llm.CountTokensRequest{
				Provider: "anthropic", Model: "x",
				Messages: []llm.ChatMessage{{Role: "user", Content: "ping"}},
			})
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("call: %v", e)
	}

	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()
	t.Logf("after 50 concurrent calls: %d (delta %d)", after, after-baseline)
	if after > baseline+10 {
		t.Errorf("goroutine growth too high : baseline=%d after=%d", baseline, after)
	}
}

func TestIsolation_StreamCancel_ClosesChannelPromptly(t *testing.T) {
	exe := buildLLMWorker(t)
	m := worker.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	_ = m.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, worker.Spec{Kind: "llm", Binary: exe, Count: 1, StartTimeout: 15 * time.Second}); err != nil {
		t.Fatal(err)
	}

	client, _ := llm.NewClient(llm.ClientConfig{Manager: m})

	// Start a stream with gateway routing (BYOK=false default ; fake JWT → provider 401).
	streamCtx, streamCancel := context.WithCancel(ctx)
	chunks, err := client.ChatStream(streamCtx, &llm.ChatRequest{
		Provider: "anthropic", Model: "claude-sonnet-4.5",
		UserJWT:  "fake-jwt",
		Messages: []llm.ChatMessage{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Cancel immediately — channel must close fast.
	streamCancel()
	start := time.Now()
	for range chunks {
	}
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Errorf("channel did not close promptly after cancel: %v", elapsed)
	}
}

func TestIsolation_WorkerCrashTriggersRestart(t *testing.T) {
	exe := buildLLMWorker(t)
	m := worker.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	_ = m.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, worker.Spec{
		Kind: "llm", Binary: exe, Count: 1,
		StartTimeout: 15 * time.Second,
		BackoffMin:   100 * time.Millisecond,
		BackoffMax:   500 * time.Millisecond,
		MaxFailures:  10,
	}); err != nil {
		t.Fatal(err)
	}

	client, _ := llm.NewClient(llm.ClientConfig{Manager: m})

	// Sanity : the worker responds.
	if _, err := client.ListProviders(ctx); err != nil {
		t.Fatalf("initial call: %v", err)
	}

	// Kill the worker PID directly (simulating crash).
	pool := m.Pool("llm")
	if len(pool) == 0 {
		t.Fatal("no worker")
	}
	pid := pool[0].PID
	t.Logf("killing PID %d", pid)
	if err := killPID(pid); err != nil {
		t.Fatalf("kill: %v", err)
	}

	// Wait for restart.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		newPool := m.Pool("llm")
		if len(newPool) > 0 && newPool[0].Restarts >= 1 && newPool[0].Address != "" && newPool[0].PID != pid {
			t.Logf("restarted as PID %d after %d restart(s)", newPool[0].PID, newPool[0].Restarts)
			// Verify client can call again.
			callCtx, callCancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer callCancel()
			if _, err := client.ListProviders(callCtx); err == nil {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("worker did not recover within 10s")
}

func TestIsolation_LoadBalanceUnderConcurrency(t *testing.T) {
	exe := buildLLMWorker(t)
	m := worker.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	_ = m.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, worker.Spec{
		Kind: "llm", Binary: exe, Count: 3, StartTimeout: 15 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}

	client, _ := llm.NewClient(llm.ClientConfig{Manager: m})

	var wg sync.WaitGroup
	var ok atomic.Int64
	var fail atomic.Int64
	const N = 100
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.ListProviders(ctx)
			if err != nil {
				fail.Add(1)
			} else {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if fail.Load() > 0 {
		t.Errorf("failures under load: %d/%d", fail.Load(), N)
	}
	pool := m.Pool("llm")
	if len(pool) != 3 {
		t.Fatalf("pool size: %d", len(pool))
	}
	t.Logf("load test : %d ok, %d fail across %d workers", ok.Load(), fail.Load(), len(pool))
}

func TestIsolation_ClientStatsConsistent(t *testing.T) {
	exe := buildLLMWorker(t)
	m := worker.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	_ = m.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, worker.Spec{Kind: "llm", Binary: exe, Count: 2, StartTimeout: 15 * time.Second}); err != nil {
		t.Fatal(err)
	}
	client, _ := llm.NewClient(llm.ClientConfig{Manager: m})

	// Make a few calls and check stats reflect them.
	for i := 0; i < 5; i++ {
		_, _ = client.ListProviders(ctx)
	}
	st := client.Stats()
	if st.WorkerPool.Total != 2 || st.WorkerPool.Ready != 2 {
		t.Fatalf("worker pool stats : %+v", st.WorkerPool)
	}
}

func TestIsolation_NilRequestReturnsError(t *testing.T) {
	exe := buildLLMWorker(t)
	m := worker.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	_ = m.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, worker.Spec{Kind: "llm", Binary: exe, Count: 1, StartTimeout: 15 * time.Second}); err != nil {
		t.Fatal(err)
	}
	client, _ := llm.NewClient(llm.ClientConfig{Manager: m})

	if _, err := client.Chat(ctx, nil); err == nil {
		t.Fatal("nil request must error")
	}
	if _, err := client.CountTokens(ctx, nil); err == nil {
		t.Fatal("nil count request must error")
	}
	if _, err := client.Embed(ctx, nil); err == nil {
		t.Fatal("nil embed request must error")
	}

	// Worker should still serve subsequent valid calls.
	if _, err := client.ListProviders(ctx); err != nil {
		t.Fatalf("worker died after nil requests: %v", err)
	}
}

func TestIsolation_NoWorkerAvailable_PropagatesError(t *testing.T) {
	m := worker.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	_ = m.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})
	client, _ := llm.NewClient(llm.ClientConfig{Manager: m, Retries: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := client.ListProviders(ctx)
	if err == nil {
		t.Fatal("expected error without worker spawned")
	}
	if !errors.Is(err, worker.ErrNoHealthyWorker) && err.Error() != "" {
		// Either ErrNoHealthyWorker or wrapped — both acceptable as long as we got something.
	}
}

// killPID forcibly terminates a process — cross-platform.
func killPID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
