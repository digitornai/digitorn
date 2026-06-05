package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
)

// TestManager_ConnPool_RoundRobinAcrossSlots verifies that successive
// ensureConn calls on the same managedConn return different conns from
// the per-worker pool. Without round-robin every HTTP/2 stream would
// multiplex over one conn and hit MaxConcurrentStreams.
func TestManager_ConnPool_RoundRobinAcrossSlots(t *testing.T) {
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
	if err := m.Spawn(ctx, Spec{Kind: "dummy", Binary: exe, Count: 1}); err != nil {
		t.Fatal(err)
	}

	m.mu.RLock()
	p := m.pools["dummy"]
	m.mu.RUnlock()
	if p == nil || len(p.clients) != 1 {
		t.Fatalf("expected one worker, got %d", len(p.clients))
	}
	mc := p.clients[0]

	// 4×poolSize calls — expect ≥ 2 distinct conns. Perfect round-robin
	// would give exactly `connPoolSize`, but slot-skip on transient state
	// can drop the count slightly.
	seen := map[*grpc.ClientConn]int{}
	for i := 0; i < 4*connPoolSize; i++ {
		c, err := mc.ensureConn(ctx)
		if err != nil {
			t.Fatalf("ensureConn[%d]: %v", i, err)
		}
		seen[c]++
	}
	if len(seen) < 2 {
		t.Errorf("expected ≥ 2 distinct conns across the round-robin pool (size %d), got %d", connPoolSize, len(seen))
	}
	t.Logf("distinct conns: %d / poolSize=%d", len(seen), connPoolSize)
}

// TestManager_ConnPool_LockFreeUnderLoad fires N goroutines at the same
// managedConn. Pre-refactor this serialised on managedConn.mu; post-
// refactor the hot path is atomic-only.
func TestManager_ConnPool_LockFreeUnderLoad(t *testing.T) {
	exe := buildDummyWorker(t)
	m := NewManager(quietLogger())
	_ = m.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, Spec{Kind: "dummy", Binary: exe, Count: 1}); err != nil {
		t.Fatal(err)
	}

	m.mu.RLock()
	p := m.pools["dummy"]
	m.mu.RUnlock()
	mc := p.clients[0]
	// Warm every slot so the per-slot mutex isn't on the timed path.
	for i := 0; i < connPoolSize; i++ {
		if _, err := mc.ensureConn(ctx); err != nil {
			t.Fatal(err)
		}
	}

	const N = 1000
	var wg sync.WaitGroup
	var failures atomic.Int32
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := mc.ensureConn(ctx); err != nil {
				failures.Add(1)
			}
		}()
	}
	t0 := time.Now()
	close(start)
	wg.Wait()
	elapsed := time.Since(t0)

	if f := failures.Load(); f > 0 {
		t.Errorf("%d/%d ensureConn calls failed under concurrency", f, N)
	}
	t.Logf("%d concurrent ensureConn calls in %s (%.0f ns/call)",
		N, elapsed, float64(elapsed.Nanoseconds())/float64(N))
}

// TestManager_PickReturnsPinnedConn confirms Pick returns a pickedConn
// whose GRPC() is stable across calls. Without pinning, the caller's
// GRPC() would round-robin into a different slot than the one Pick
// state-checked.
func TestManager_PickReturnsPinnedConn(t *testing.T) {
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
	if err := m.Spawn(ctx, Spec{Kind: "dummy", Binary: exe, Count: 1}); err != nil {
		t.Fatal(err)
	}

	c, err := m.Pick(ctx, "dummy")
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	first := c.GRPC()
	if first == nil {
		t.Fatal("pickedConn.GRPC() returned nil")
	}
	for i := 0; i < 10; i++ {
		if got := c.GRPC(); got != first {
			t.Errorf("pickedConn.GRPC() drifted on call %d", i)
		}
	}
}
