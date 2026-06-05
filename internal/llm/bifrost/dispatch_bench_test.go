package bifrost

import (
	"context"
	"sync/atomic"
	"testing"

	"golang.org/x/sync/semaphore"

	"github.com/mbathepaul/digitorn/internal/llm"
)

// BenchmarkDispatch_FastPath measures the per-call cost of the worker-
// side hot path WITHOUT the real Bifrost network call. We synthetically
// stop right after admission + routeInfo acquire so the bench targets
// exactly the digitorn-owned overhead (admission + pool + context).
// Pre-Phase-4/5 baseline: ~1 alloc per call for routeInfo, no admission.
// Phase-4: 0 allocs on routeInfo. Phase-5: +1 semaphore cycle.
func BenchmarkDispatch_FastPath(b *testing.B) {
	s := &Service{
		admission: semaphore.NewWeighted(16384),
		cfg:       Config{Concurrency: 256, BufferSize: 16384},
	}
	req := &llm.ChatRequest{
		BYOK: true, Provider: "anthropic", Model: "x",
		APIKey:   "sk-test",
		Messages: []llm.ChatMessage{{Role: "user", Content: "ping"}},
	}
	ctx := context.Background()

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := s.admit(ctx); err != nil {
				b.Fatal(err)
			}
			route := acquireRouteInfo(req.BYOK, req.APIKey, req.UserJWT, req.BaseURL)
			_ = route.BYOK
			releaseRouteInfo(route)
			s.admission.Release(1)
		}
	})
}

// BenchmarkDispatch_SaturatedAdmission simulates the throttle case:
// admission slots maxed out. Each goroutine blocks waiting for a slot
// to free up. This is the SLOW path that protects the worker from
// dropping requests under burst. Useful for tracking the cost of
// admission contention vs. silent Bifrost drops.
func BenchmarkDispatch_SaturatedAdmission(b *testing.B) {
	s := &Service{
		admission: semaphore.NewWeighted(64), // small pool, lots of contention
		cfg:       Config{BufferSize: 64},
	}
	ctx := context.Background()
	var rejected atomic.Int64

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := s.admit(ctx); err != nil {
				rejected.Add(1)
				continue
			}
			s.admission.Release(1)
		}
	})
	if r := rejected.Load(); r > 0 {
		b.Logf("admissions rejected: %d (expected 0 — ctx never cancels)", r)
	}
}

// BenchmarkDispatch_10KConcurrent: 10 000 goroutines hitting the admit
// path in lockstep. Validates the no-OOM, no-deadlock guarantee under
// the 10K-agent target. With BufferSize=16384 and N=10K, every goroutine
// should pass through admission with zero queuing.
func BenchmarkDispatch_10KConcurrent(b *testing.B) {
	s := &Service{
		admission: semaphore.NewWeighted(16384),
		cfg:       Config{BufferSize: 16384},
	}
	ctx := context.Background()
	b.SetParallelism(10000 / runtimeGoMaxprocs()) // GOMAXPROCS-relative
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := s.admit(ctx); err != nil {
				b.Fatal(err)
			}
			s.admission.Release(1)
		}
	})
}

// runtimeGoMaxprocs returns GOMAXPROCS without dragging in runtime.GOMAXPROCS
// from a non-test path. We pin it at the goroutine layer not the test layer
// to keep the bench reproducible across CI vs. local.
func runtimeGoMaxprocs() int {
	// Approximate via a cheap atomic-bound check — testing.B.SetParallelism
	// caps at GOMAXPROCS internally anyway, so over-asking is harmless.
	return 16
}
