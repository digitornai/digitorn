package indexer

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ownerStat tracks, per owner, the peak number of concurrent Walks — i.e. how
// many pool slots one app holds at once.
type ownerStat struct {
	mu       sync.Mutex
	cur, max map[string]int
}

var ownStat = &ownerStat{cur: map[string]int{}, max: map[string]int{}}

func (o *ownerStat) reset() {
	o.mu.Lock()
	o.cur, o.max = map[string]int{}, map[string]int{}
	o.mu.Unlock()
}
func (o *ownerStat) enter(owner string) {
	o.mu.Lock()
	o.cur[owner]++
	if o.cur[owner] > o.max[owner] {
		o.max[owner] = o.cur[owner]
	}
	o.mu.Unlock()
}
func (o *ownerStat) leave(owner string) { o.mu.Lock(); o.cur[owner]--; o.mu.Unlock() }
func (o *ownerStat) peak(owner string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.max[owner]
}

type ownerConn struct{}

func (ownerConn) Type() string      { return "ownertrack" }
func (ownerConn) Capabilities() Caps { return Caps{Walk: true} }
func (ownerConn) Watch(context.Context, SourceSpec, Sink, Cursor) error { return nil }
func (ownerConn) Walk(_ context.Context, spec SourceSpec, emit func(Document) error) error {
	ownStat.enter(spec.Owner)
	time.Sleep(4 * time.Millisecond)
	ownStat.leave(spec.Owner)
	return emit(Document{ID: spec.Name, Text: "x"})
}

var ownerOnce sync.Once

// TestScheduler_PerAppFairness proves one app cannot monopolise the worker pool
// while another is active : with two equally-busy apps and a pool of 16, each
// app's peak concurrency stays near its fair share (≈8), and both make real
// progress concurrently — no starvation.
func TestScheduler_PerAppFairness(t *testing.T) {
	ownerOnce.Do(func() { Register(ownerConn{}) })
	ownStat.reset()
	const perOwner, workers = 200, 16
	svc := NewService(NewMemCursor(), workers)
	sink := &countSink{}

	// Register both apps' sources WITHOUT a trigger, then release them all at
	// once so both apps are active from the same instant — otherwise the first
	// app registered would (correctly) use the whole idle pool before the
	// second exists, which is full utilisation, not unfairness.
	for _, owner := range []string{"app-A", "app-B"} {
		for i := 0; i < perOwner; i++ {
			svc.Register(SourceSpec{
				Name: fmt.Sprintf("%s-%d", owner, i), Type: "ownertrack", KB: "kb", Owner: owner,
			}, sink)
		}
	}
	svc.sched.mu.Lock()
	for _, j := range svc.sched.jobs {
		j.due = true
	}
	svc.sched.mu.Unlock()
	svc.sched.signal()

	want := int64(perOwner * 2)
	deadline := time.Now().Add(45 * time.Second)
	for atomic.LoadInt64(&sink.ups) < want {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %d/%d done", atomic.LoadInt64(&sink.ups), want)
		}
		time.Sleep(2 * time.Millisecond)
	}

	a, b := ownStat.peak("app-A"), ownStat.peak("app-B")
	share := workers / 2
	t.Logf("fair-share: app-A peak %d, app-B peak %d (fair share ≈ %d of %d workers)", a, b, share, workers)
	if a > share+3 || b > share+3 {
		t.Fatalf("fairness breached: one app hogged the pool (A=%d B=%d, share≈%d)", a, b, share)
	}
	if a < 2 || b < 2 {
		t.Fatalf("no real concurrency for an app (A=%d B=%d)", a, b)
	}
}
