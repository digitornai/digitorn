package approval_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/runtime/policy/approval"
)

// TestRegistry_ResolveBeforeWait_LeavesDanglingResolution : the
// Resolve must be a no-op when no waiter is registered. The
// registry doesn't keep a backlog of unresolved arrivals — that
// would invite memory leaks if the daemon receives spurious
// HTTP /approve calls for nonexistent IDs.
func TestRegistry_ResolveBeforeWait_NoOp(t *testing.T) {
	r := approval.NewRegistry()
	ok := r.Resolve("nonexistent", approval.Resolution{Result: approval.ResultApproved})
	if ok {
		t.Fatal("Resolve on unknown id should return false")
	}
	if got := r.Pending(); got != 0 {
		t.Fatalf("Pending = %d, want 0", got)
	}
}

// TestRegistry_Wait_Approved : the happy path. Register a waiter,
// resolve it, Wait returns Approved.
func TestRegistry_Wait_Approved(t *testing.T) {
	r := approval.NewRegistry()
	done := make(chan approval.Resolution, 1)

	go func() {
		done <- r.Wait(context.Background(), "req-1", 0)
	}()

	// Give the goroutine time to register.
	deadline := time.Now().Add(time.Second)
	for r.Pending() == 0 && time.Now().Before(deadline) {
		time.Sleep(1 * time.Millisecond)
	}
	if r.Pending() != 1 {
		t.Fatalf("Pending = %d, want 1", r.Pending())
	}

	ok := r.Resolve("req-1", approval.Resolution{
		Result: approval.ResultApproved,
		Reason: "user clicked approve",
	})
	if !ok {
		t.Fatal("Resolve returned false despite registered waiter")
	}

	select {
	case res := <-done:
		if res.Result != approval.ResultApproved {
			t.Errorf("result = %v, want approved", res.Result)
		}
		if res.Reason != "user clicked approve" {
			t.Errorf("reason = %q, want 'user clicked approve'", res.Reason)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Wait didn't return after Resolve")
	}

	if got := r.Pending(); got != 0 {
		t.Fatalf("Pending = %d after Wait returned, want 0 (cleanup)", got)
	}
}

// TestRegistry_Wait_Denied : the denial path. Wait returns Denied
// with the supplied reason.
func TestRegistry_Wait_Denied(t *testing.T) {
	r := approval.NewRegistry()
	done := make(chan approval.Resolution, 1)
	go func() {
		done <- r.Wait(context.Background(), "req-2", 0)
	}()

	for r.Pending() == 0 {
		time.Sleep(1 * time.Millisecond)
	}
	r.Resolve("req-2", approval.Resolution{
		Result: approval.ResultDenied,
		Reason: "too risky in this session",
	})

	res := <-done
	if res.Result != approval.ResultDenied {
		t.Fatalf("result = %v, want denied", res.Result)
	}
	if res.Reason != "too risky in this session" {
		t.Errorf("reason = %q", res.Reason)
	}
}

// TestRegistry_Wait_Timeout : when no resolution arrives within the
// timeout, the result is Timeout. Doc says auto-deny ; the engine
// caller will translate this into an "auto_denied" event.
func TestRegistry_Wait_Timeout(t *testing.T) {
	r := approval.NewRegistry()
	t0 := time.Now()
	res := r.Wait(context.Background(), "req-3", 50*time.Millisecond)
	dur := time.Since(t0)

	if res.Result != approval.ResultTimeout {
		t.Fatalf("result = %v, want timeout", res.Result)
	}
	if dur < 40*time.Millisecond {
		t.Errorf("Wait returned too fast : %v (want >= 40ms)", dur)
	}
	if dur > 200*time.Millisecond {
		t.Errorf("Wait took too long : %v (want < 200ms)", dur)
	}
	if r.Pending() != 0 {
		t.Errorf("waiter not cleaned up after timeout")
	}
}

// TestRegistry_Wait_CtxCancel : Wait returns Cancelled when the
// caller's context is cancelled. The audit row will surface this as
// "interrupted" rather than "denied".
func TestRegistry_Wait_CtxCancel(t *testing.T) {
	r := approval.NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan approval.Resolution, 1)
	go func() {
		done <- r.Wait(ctx, "req-4", 5*time.Second)
	}()

	for r.Pending() == 0 {
		time.Sleep(1 * time.Millisecond)
	}
	cancel()

	select {
	case res := <-done:
		if res.Result != approval.ResultCancelled {
			t.Fatalf("result = %v, want cancelled", res.Result)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Wait didn't return after ctx cancel")
	}
	if r.Pending() != 0 {
		t.Errorf("waiter not cleaned up after cancel")
	}
}

// TestRegistry_DoubleResolve_SecondNoOp : two Resolve calls for the
// same id : the first wins, the second is a no-op (returns false).
// Prevents a race between an HTTP approve and a timeout from
// double-firing the waiter.
func TestRegistry_DoubleResolve_SecondNoOp(t *testing.T) {
	r := approval.NewRegistry()
	done := make(chan approval.Resolution, 1)
	go func() {
		done <- r.Wait(context.Background(), "req-5", 0)
	}()
	for r.Pending() == 0 {
		time.Sleep(1 * time.Millisecond)
	}

	ok1 := r.Resolve("req-5", approval.Resolution{Result: approval.ResultApproved})
	if !ok1 {
		t.Fatal("first Resolve returned false")
	}
	<-done // ensure the waiter consumed it

	ok2 := r.Resolve("req-5", approval.Resolution{Result: approval.ResultDenied})
	if ok2 {
		t.Fatal("second Resolve returned true (should be no-op)")
	}
}

// TestRegistry_ConcurrentWaiters : N concurrent waiters, each
// resolved independently. Verifies no cross-talk and proper cleanup.
func TestRegistry_ConcurrentWaiters(t *testing.T) {
	const N = 50
	r := approval.NewRegistry()

	results := make([]approval.Result, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			res := r.Wait(context.Background(), id(i), 0)
			results[i] = res.Result
		}(i)
	}

	// Wait for all to register.
	deadline := time.Now().Add(2 * time.Second)
	for r.Pending() != N && time.Now().Before(deadline) {
		time.Sleep(1 * time.Millisecond)
	}
	if r.Pending() != N {
		t.Fatalf("Pending = %d, want %d", r.Pending(), N)
	}

	// Resolve half approved, half denied.
	for i := 0; i < N; i++ {
		want := approval.ResultApproved
		if i%2 == 1 {
			want = approval.ResultDenied
		}
		r.Resolve(id(i), approval.Resolution{Result: want})
	}

	wg.Wait()

	for i := 0; i < N; i++ {
		want := approval.ResultApproved
		if i%2 == 1 {
			want = approval.ResultDenied
		}
		if results[i] != want {
			t.Errorf("waiter %d : got %v, want %v", i, results[i], want)
		}
	}
	if r.Pending() != 0 {
		t.Errorf("Pending = %d after all resolved, want 0", r.Pending())
	}
}

// TestRegistry_NilSafe : a nil receiver is allowed and degrades to
// "no enforcement" : Wait returns Cancelled immediately, Resolve
// returns false. Lets callers wire optional enforcement without
// extra nil-checks.
func TestRegistry_NilSafe(t *testing.T) {
	var r *approval.Registry
	res := r.Wait(context.Background(), "req-x", 0)
	if res.Result != approval.ResultCancelled {
		t.Fatalf("nil Registry Wait : got %v, want Cancelled", res.Result)
	}
	if r.Resolve("req-x", approval.Resolution{}) {
		t.Fatal("nil Registry Resolve : got true, want false")
	}
	if r.Pending() != 0 {
		t.Fatal("nil Registry Pending must be 0")
	}
}

func id(i int) string {
	return "req-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [10]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
