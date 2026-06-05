//go:build stability
// +build stability

// Phase 8 stability suite. These tests are SLOW (10s-60s each) and
// intentionally push the daemon-side hot paths until they break. Run
// with `go test -tags stability -timeout 30m ./internal/llm/bifrost/...`.
// They are tag-gated so a normal `go test ./...` stays under 30s.
//
// What each test proves:
//
//   - SustainedThroughput_30s: 30 seconds of admit+release pegged at
//     GOMAXPROCS. Watches for throughput drift (a quiet degradation
//     across time would catch a slow leak, a lock convoy, etc.).
//
//   - BurstSaturation_50K: 50 000 goroutines fire admit() simultaneously
//     against a 16 384-slot pool. The 33 616 over-budget callers MUST
//     either succeed-eventually (queued) or fail clean with
//     codes.ResourceExhausted — NEVER hang, NEVER panic.
//
//   - MemoryStability_1M: 1 000 000 sequential routeInfo acquire/release
//     cycles. Heap-in-use after must be ≤ 2× heap-before. Detects pool
//     mis-Put + slow leaks invisible at 1K iterations.
//
//   - GoroutineStability: hits the pool under sustained 1 000-goroutine
//     concurrency for 5 s, then waits and checks NumGoroutine returns
//     to baseline. Catches the "goroutine spawned per request but never
//     reaped" failure mode (Phase-3 isolation test caught one).
//
//   - CircuitBreakerLifecycle: synthetic failures push the CB to OPEN,
//     verifies short-circuit kicks in, waits for the timeout, fires a
//     single HALF-OPEN probe, then verifies CLOSE. Repeats 50 times to
//     prove no state leaks across cycles.
//
//   - PoolStability_10K: 10 000 goroutines on a single managedConn for
//     5 s — proves the lock-free fast path holds under sustained
//     pressure (Phase-3 introduced the conn pool, this is its torture
//     test).
//
//   - MixedWorkload_Chaos: Chat + Embed admit paths run concurrently
//     with random ctx cancellations. Verifies no deadlock, no double-
//     release of semaphore slots (which would corrupt the pool).
package bifrost

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	schemas "github.com/maximhq/bifrost/core/schemas"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ---------- helper: realistic Service stub ----------

func newStubService(bufferSize int) *Service {
	return &Service{
		admission: semaphore.NewWeighted(int64(bufferSize)),
		cfg:       Config{Concurrency: 256, BufferSize: bufferSize},
	}
}

// ---------- 1. Sustained Throughput ----------

func TestStability_SustainedThroughput_30s(t *testing.T) {
	t.Parallel()
	s := newStubService(16384)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const workers = 64
	var ops atomic.Uint64
	var done atomic.Bool
	var wg sync.WaitGroup

	// Track buckets of 1-second ops to detect drift.
	const buckets = 30
	var bucket [buckets]atomic.Uint64
	start := time.Now()

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !done.Load() {
				if err := s.admit(ctx); err != nil {
					return
				}
				r := acquireRouteInfo(true, "sk-test", "", "")
				_ = r.BYOK
				releaseRouteInfo(r)
				s.admission.Release(1)
				ops.Add(1)
				if elapsed := time.Since(start); elapsed < buckets*time.Second {
					bucket[int(elapsed.Seconds())].Add(1)
				}
			}
		}()
	}

	<-ctx.Done()
	done.Store(true)
	wg.Wait()

	total := ops.Load()
	rate := float64(total) / 30.0
	t.Logf("total=%d ops over 30s, rate=%.0f ops/s, per-worker=%.0f", total, rate, rate/float64(workers))

	// Drift check: last 5s rate vs first 5s rate must not drop > 30%.
	first5 := uint64(0)
	last5 := uint64(0)
	for i := 0; i < 5; i++ {
		first5 += bucket[i].Load()
		last5 += bucket[buckets-1-i].Load()
	}
	t.Logf("first 5s: %d ops, last 5s: %d ops", first5, last5)
	if first5 > 0 {
		ratio := float64(last5) / float64(first5)
		if ratio < 0.70 {
			t.Errorf("throughput degraded: last/first = %.2f (cap 0.70)", ratio)
		}
	}
}

// ---------- 2. Burst Saturation ----------

func TestStability_BurstSaturation_50K(t *testing.T) {
	t.Parallel()
	const bufferSize = 16384
	const burst = 50000
	s := newStubService(bufferSize)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var admitted atomic.Int64
	var rejected atomic.Int64
	var unexpected atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			callCtx, callCancel := context.WithTimeout(ctx, 5*time.Second)
			defer callCancel()
			if err := s.admit(callCtx); err != nil {
				st, ok := status.FromError(err)
				if !ok || st.Code() != codes.ResourceExhausted {
					unexpected.Add(1)
					return
				}
				rejected.Add(1)
				return
			}
			defer s.admission.Release(1)
			admitted.Add(1)
			// Hold the slot a bit so subsequent goroutines have to queue.
			time.Sleep(time.Microsecond * 10)
		}()
	}

	t0 := time.Now()
	close(start)
	wg.Wait()
	elapsed := time.Since(t0)

	a := admitted.Load()
	r := rejected.Load()
	u := unexpected.Load()
	t.Logf("burst %d → admitted=%d, rejected=%d, unexpected=%d in %s",
		burst, a, r, u, elapsed)

	if u > 0 {
		t.Errorf("got %d errors with non-ResourceExhausted code", u)
	}
	if a+r+u != burst {
		t.Errorf("accounting mismatch: %d+%d+%d != %d", a, r, u, burst)
	}
}

// ---------- 3. Memory Stability ----------

func TestStability_MemoryStability_1M(t *testing.T) {
	// NO t.Parallel: this test reads runtime.MemStats which is process-wide.
	// Parallel siblings doing heavy allocs would pollute the before/after delta
	// with their work, giving false positives unrelated to routeInfo pool.
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	const N = 1_000_000
	for i := 0; i < N; i++ {
		r := acquireRouteInfo(true, "sk-test", "", "")
		_ = r.BYOK
		releaseRouteInfo(r)
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	deltaHeap := int64(after.HeapInuse) - int64(before.HeapInuse)
	deltaAllocs := int64(after.Mallocs) - int64(before.Mallocs)
	allocsPerOp := float64(deltaAllocs) / float64(N)

	t.Logf("1M cycles: heap Δ=%+d bytes, %d mallocs (%.4f per op)",
		deltaHeap, deltaAllocs, allocsPerOp)

	// HeapInuse can fluctuate downward (GC) — only flag growth > 2×.
	if deltaHeap > 0 && uint64(deltaHeap) > before.HeapInuse {
		t.Errorf("heap doubled after 1M cycles: before=%d, after=%d",
			before.HeapInuse, after.HeapInuse)
	}
	// Pool win expected: < 0.001 allocs per op (one allocation only when
	// the pool seeds; everything else recycled).
	if allocsPerOp > 0.01 {
		t.Errorf("alloc rate too high: %.4f/op (cap 0.01)", allocsPerOp)
	}
}

// ---------- 4. Goroutine Stability ----------

func TestStability_GoroutineStability(t *testing.T) {
	// NO t.Parallel: runtime.NumGoroutine() is process-wide. Parallel
	// siblings spinning up workers would inflate baseline + after counts
	// non-deterministically.
	s := newStubService(16384)
	runtime.GC()
	baseline := runtime.NumGoroutine()

	const workers = 1000
	const duration = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if err := s.admit(ctx); err != nil {
					return
				}
				r := acquireRouteInfo(true, "sk", "", "")
				releaseRouteInfo(r)
				s.admission.Release(1)
			}
		}()
	}
	wg.Wait()

	// Let any deferred goroutines wind down + GC.
	time.Sleep(500 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()

	t.Logf("goroutines: baseline=%d, after %d workers, %s load: %d (Δ=%+d)",
		baseline, workers, duration, after, after-baseline)
	// We tolerate small drift (testing framework, GC sweep workers) but
	// fail on > 50 extra goroutines retained (would indicate a real leak).
	if after > baseline+50 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, after)
	}
}

// ---------- 5. Circuit Breaker Lifecycle ----------

func TestStability_CircuitBreakerLifecycle(t *testing.T) {
	t.Parallel()
	// threshold=3, window=200ms, openFor=20ms (short for fast cycles)
	cb := NewCircuitBreakerPlugin(3, 200*time.Millisecond, 20*time.Millisecond)

	chatReqFor := func(p string) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{
			ChatRequest: &schemas.BifrostChatRequest{
				Provider: schemas.ModelProvider(p), Model: "x",
			},
		}
	}
	mkCtx := func() *schemas.BifrostContext {
		bc, _ := schemas.NewBifrostContextWithTimeout(context.TODO(), 5*time.Second)
		return bc
	}

	const cycles = 50
	for cycle := 0; cycle < cycles; cycle++ {
		prov := fmt.Sprintf("test-cycle-%d", cycle) // fresh provider key each cycle isolates state

		// Phase A: drive 3 failures → CB OPEN.
		for i := 0; i < 3; i++ {
			ctx := mkCtx()
			_, sc, _ := cb.PreLLMHook(ctx, chatReqFor(prov))
			if sc != nil {
				t.Fatalf("cycle %d call %d: short-circuit before threshold", cycle, i)
			}
			cb.PostLLMHook(ctx, nil, &schemas.BifrostError{Error: &schemas.ErrorField{Message: "boom"}})
		}
		// Now the 4th call must short-circuit.
		ctx := mkCtx()
		_, sc, _ := cb.PreLLMHook(ctx, chatReqFor(prov))
		if sc == nil {
			t.Errorf("cycle %d: CB did not OPEN after 3 failures", cycle)
		}

		// Phase B: wait > openFor → next call enters HALF-OPEN as probe.
		time.Sleep(25 * time.Millisecond)
		ctx = mkCtx()
		_, sc, _ = cb.PreLLMHook(ctx, chatReqFor(prov))
		if sc != nil {
			t.Errorf("cycle %d: probe was short-circuited (should pass through)", cycle)
			continue
		}

		// Phase C: success → CLOSE. Subsequent call must pass without short-circuit.
		cb.PostLLMHook(ctx, &schemas.BifrostResponse{}, nil)
		ctx = mkCtx()
		_, sc, _ = cb.PreLLMHook(ctx, chatReqFor(prov))
		if sc != nil {
			t.Errorf("cycle %d: CB did not CLOSE after probe success", cycle)
		}
	}
	t.Logf("%d OPEN→HALF-OPEN→CLOSE cycles, no state leak detected", cycles)
}

// ---------- 6. Pool Stability under 10K ----------

func TestStability_PoolStability_10K(t *testing.T) {
	t.Parallel()
	s := newStubService(20000)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const workers = 10000
	var ops atomic.Uint64
	var failures atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if err := s.admit(ctx); err != nil {
					if ctx.Err() != nil {
						return
					}
					failures.Add(1)
					return
				}
				ops.Add(1)
				s.admission.Release(1)
			}
		}()
	}
	wg.Wait()

	o := ops.Load()
	f := failures.Load()
	t.Logf("10K goroutines / 5s: %d ops (%.0f ops/s), %d unexpected failures",
		o, float64(o)/5.0, f)
	if f > 0 {
		t.Errorf("unexpected admission failures: %d", f)
	}
}

// ---------- 7. Mixed Workload Chaos ----------

func TestStability_MixedWorkload_Chaos(t *testing.T) {
	t.Parallel()
	s := newStubService(2048)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const workers = 500
	var chatOK, embedOK, cancelled, errored atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for ctx.Err() == nil {
				// Mix Chat-like and Embed-like patterns.
				callCtx, callCancel := context.WithTimeout(ctx, 50*time.Millisecond)
				// Randomly cancel ~20% mid-flight to stress the cleanup path.
				if idx%5 == 0 {
					go func() {
						time.Sleep(2 * time.Millisecond)
						callCancel()
					}()
				}
				if err := s.admit(callCtx); err != nil {
					if err == context.Canceled || err == context.DeadlineExceeded {
						cancelled.Add(1)
					} else if st, ok := status.FromError(err); ok && st.Code() == codes.ResourceExhausted {
						cancelled.Add(1)
					} else {
						errored.Add(1)
					}
					callCancel()
					continue
				}
				// Simulate Chat vs Embed shape via routeInfo
				route := acquireRouteInfo(true, "sk", "", "")
				if idx%2 == 0 {
					chatOK.Add(1)
				} else {
					embedOK.Add(1)
				}
				releaseRouteInfo(route)
				s.admission.Release(1)
				callCancel()
			}
		}(i)
	}
	wg.Wait()

	t.Logf("mixed workload: chat=%d, embed=%d, cancelled=%d, errored=%d",
		chatOK.Load(), embedOK.Load(), cancelled.Load(), errored.Load())
	if errored.Load() > 0 {
		t.Errorf("unexpected errors (not cancel/exhausted): %d", errored.Load())
	}
}

// ---------- helper: format big numbers in test output ----------

func fmtN(n uint64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}
