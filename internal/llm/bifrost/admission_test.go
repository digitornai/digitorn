package bifrost

import (
	"context"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestAdmissionControl_ReturnsResourceExhaustedOnContextCancel proves
// the admit() helper respects ctx cancellation and surfaces a clean
// codes.ResourceExhausted error rather than blocking forever.
//
// Why this matters: pre-Phase-5, the Bifrost InitialPoolSize buffer
// silently dropped requests above the cap (DropExcessRequests=true).
// Admission control replaces silent drops with explicit, retryable
// errors — and the daemon's isRetryable() (Phase 1) already treats
// codes.ResourceExhausted as retry-once.
func TestAdmissionControl_ReturnsResourceExhaustedOnContextCancel(t *testing.T) {
	// Build a Service with a tiny admission semaphore so we can saturate
	// it without standing up the full Bifrost client. We construct the
	// struct directly because NewService does network/I/O setup we don't
	// need for this contract.
	s := &Service{
		admission: semaphore.NewWeighted(1),
		cfg:       Config{BufferSize: 1},
	}

	// Grab the only slot ourselves to simulate a saturated worker.
	if err := s.admission.Acquire(context.Background(), 1); err != nil {
		t.Fatalf("baseline acquire: %v", err)
	}
	// Don't release — next admit() must hit ctx cancellation.

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := s.admit(ctx)
	if err == nil {
		t.Fatal("expected admission denial when saturated + ctx expired")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %T: %v", err, err)
	}
	if st.Code() != codes.ResourceExhausted {
		t.Errorf("code = %v, want ResourceExhausted (retryable)", st.Code())
	}
}

// TestAdmissionControl_FastPath_When_SlotsAvailable confirms admit()
// is sub-microsecond when the pool is wide open — no goroutine yield,
// no allocation, just an atomic.
func TestAdmissionControl_FastPath_When_SlotsAvailable(t *testing.T) {
	s := &Service{
		admission: semaphore.NewWeighted(1024),
		cfg:       Config{BufferSize: 1024},
	}

	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 1000; i++ {
		if err := s.admit(ctx); err != nil {
			t.Fatalf("admit[%d]: %v", i, err)
		}
		s.admission.Release(1)
	}
	elapsed := time.Since(start)
	perCall := elapsed / 1000
	t.Logf("1000× admit+release: %s total, %s/call", elapsed, perCall)
	// On a modern CPU semaphore.Acquire+Release of an uncontested slot
	// is well under 1µs. We use a generous bound (10µs) to avoid CI
	// flakiness while still catching catastrophic regressions.
	if perCall > 10*time.Microsecond {
		t.Errorf("admit fast path too slow: %s/call", perCall)
	}
}

// TestAdmissionControl_ConcurrentSaturation: spin up N+1 goroutines on
// a pool of N. The first N must succeed, the last must wait until one
// releases. No goroutine should leak.
func TestAdmissionControl_ConcurrentSaturation(t *testing.T) {
	const N = 8
	s := &Service{
		admission: semaphore.NewWeighted(N),
		cfg:       Config{BufferSize: N},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	hold := make(chan struct{})
	successes := make(chan struct{}, N+1)

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.admit(ctx); err != nil {
				t.Errorf("admit failed: %v", err)
				return
			}
			successes <- struct{}{}
			<-hold
			s.admission.Release(1)
		}()
	}

	// Wait for all N to hold their slot.
	for i := 0; i < N; i++ {
		<-successes
	}

	// The (N+1)-th caller must NOT succeed while all N slots are held.
	// We give it a 50ms grace window before declaring blocked.
	blocked := make(chan error, 1)
	go func() {
		tightCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		blocked <- s.admit(tightCtx)
	}()
	if err := <-blocked; err == nil {
		t.Error("N+1 admit unexpectedly succeeded while N slots held")
	}

	// Release the N holders.
	close(hold)
	wg.Wait()
}
