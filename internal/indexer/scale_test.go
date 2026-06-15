package indexer

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// loadConn is a fast in-memory connector for load + concurrency tests : each
// Walk emits opts["docs"] documents with per-source unique ids. Registered
// once under type "loadfake".
type loadConn struct{}

func (loadConn) Type() string      { return "loadfake" }
func (loadConn) Capabilities() Caps { return Caps{Walk: true} }
func (loadConn) Watch(context.Context, SourceSpec, Sink, Cursor) error { return nil }
func (loadConn) Walk(ctx context.Context, spec SourceSpec, emit func(Document) error) error {
	n, _ := optInt(spec.Opts, "docs")
	for i := 0; i < n; i++ {
		if err := emit(Document{ID: fmt.Sprintf("%s-%d", spec.Name, i), Text: "payload"}); err != nil {
			return err
		}
	}
	return nil
}

var loadOnce sync.Once

func registerLoad() { loadOnce.Do(func() { Register(loadConn{}) }) }

// countSink counts upserts and tracks the peak number of concurrent Upsert
// calls — which equals the peak number of concurrent syncs, so it must never
// exceed the scheduler's maxConcurrent bound.
type countSink struct {
	ups         int64
	inflight    int32
	maxInflight int32
	delay       time.Duration
}

func (c *countSink) Upsert(_ context.Context, _ string, docs []Document) error {
	n := atomic.AddInt32(&c.inflight, 1)
	for {
		old := atomic.LoadInt32(&c.maxInflight)
		if n <= old || atomic.CompareAndSwapInt32(&c.maxInflight, old, n) {
			break
		}
	}
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	atomic.AddInt64(&c.ups, int64(len(docs)))
	atomic.AddInt32(&c.inflight, -1)
	return nil
}

func (c *countSink) Delete(context.Context, string, string) error { return nil }

// TestService_FanOut_BoundedAndScales registers a large fan-out of sources and
// proves: every source syncs, the bounded pool is never exceeded, and it does
// so under -race. Prints real throughput numbers.
func TestService_FanOut_BoundedAndScales(t *testing.T) {
	registerLoad()
	const sources, docsEach, maxConcurrent = 3000, 5, 16
	svc := NewService(NewMemCursor(), maxConcurrent)
	sink := &countSink{delay: 1 * time.Millisecond}

	start := time.Now()
	for i := 0; i < sources; i++ {
		spec := SourceSpec{
			Name:     fmt.Sprintf("src-%d", i),
			Type:     "loadfake",
			KB:       "kb",
			Opts:     map[string]any{"docs": docsEach},
			Triggers: []Trigger{{Type: "on_start"}},
		}
		svc.Register(spec, sink)
	}

	want := int64(sources * docsEach)
	deadline := time.Now().Add(60 * time.Second)
	for atomic.LoadInt64(&sink.ups) < want {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %d/%d docs after 60s", atomic.LoadInt64(&sink.ups), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
	elapsed := time.Since(start)

	if peak := atomic.LoadInt32(&sink.maxInflight); peak > maxConcurrent {
		t.Fatalf("pool bound breached: peak concurrent syncs = %d, max = %d", peak, maxConcurrent)
	}
	t.Logf("fan-out: %d sources / %d docs in %v → %.0f sources/s, %.0f docs/s, peak concurrency %d/%d",
		sources, want, elapsed.Round(time.Millisecond),
		float64(sources)/elapsed.Seconds(), float64(want)/elapsed.Seconds(),
		atomic.LoadInt32(&sink.maxInflight), maxConcurrent)
}

// denyStore is a MemCursor that also implements Locker, refusing the lease for
// one specific key — simulating another replica already holding it.
type denyStore struct {
	*MemCursor
	deny string
}

func (d denyStore) Acquire(_ context.Context, key string) (func(), bool) {
	if key == d.deny {
		return func() {}, false
	}
	return func() {}, true
}

// TestService_DistributedLease_SkipsHeldSource proves the scheduler honors the
// distributed lease : a source whose lease is held elsewhere is never synced,
// while an unheld source is.
func TestService_DistributedLease_SkipsHeldSource(t *testing.T) {
	registerLoad()
	held := SourceSpec{Name: "held", Type: "loadfake", KB: "kb", Opts: map[string]any{"docs": 3}, Triggers: []Trigger{{Type: "on_start"}}}
	free := SourceSpec{Name: "free", Type: "loadfake", KB: "kb", Opts: map[string]any{"docs": 3}, Triggers: []Trigger{{Type: "on_start"}}}

	svc := NewService(denyStore{MemCursor: NewMemCursor(), deny: stateKey(held)}, 4)
	sink := &countSink{}
	svc.Register(held, sink)
	svc.Register(free, sink)

	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt64(&sink.ups) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("free source never synced: ups=%d", atomic.LoadInt64(&sink.ups))
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // give the held source a chance to (wrongly) run

	if got := atomic.LoadInt64(&sink.ups); got != 3 {
		t.Fatalf("lease not honored: %d docs upserted, want exactly 3 (only the free source)", got)
	}
	if skipped := svc.Stats().LeaseSkipped; skipped < 1 {
		t.Fatalf("LeaseSkipped = %d, want >= 1", skipped)
	}
}

func BenchmarkService_FanOut(b *testing.B) {
	registerLoad()
	svc := NewService(NewMemCursor(), 16)
	sink := &countSink{}
	spec := func(i int) SourceSpec {
		return SourceSpec{Name: fmt.Sprintf("b-%d", i), Type: "loadfake", KB: "kb", Opts: map[string]any{"docs": 5}}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = svc.Sync(context.Background(), spec(i), sink)
	}
}
