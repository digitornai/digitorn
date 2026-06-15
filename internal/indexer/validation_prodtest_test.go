package indexer

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// SOAK : churn thousands of register/deregister/sync cycles and assert the
// service does not leak goroutines or grow its job map unbounded — the steady
// state of a long-running daemon.
func TestService_Soak_NoLeak(t *testing.T) {
	registerLoad()
	svc := NewService(NewMemCursor(), 8)

	runtime.GC()
	g0 := runtime.NumGoroutine()
	const cycles = 3000
	for i := 0; i < cycles; i++ {
		spec := SourceSpec{
			Name: fmt.Sprintf("s-%d", i%64), Type: "loadfake", KB: "kb",
			Owner: fmt.Sprintf("app-%d", i%32), Opts: map[string]any{"docs": 3},
			Triggers: []Trigger{{Type: "on_start"}},
		}
		svc.Register(spec, &countSink{})
		if i%5 == 0 {
			svc.Deregister(spec)
		}
	}
	time.Sleep(700 * time.Millisecond) // let the bounded pool drain
	runtime.GC()
	g1 := runtime.NumGoroutine()
	jobs := svc.JobCount()
	t.Logf("soak: %d cycles → goroutines %d→%d, job map=%d", cycles, g0, g1, jobs)

	if g1 > g0+24 { // fixed worker pool + dispatcher only; not per-source
		t.Errorf("goroutine leak: %d → %d after %d cycles", g0, g1, cycles)
	}
	if jobs > 100 { // 64 distinct source keys max — must stay bounded
		t.Errorf("job map unbounded: %d", jobs)
	}
	svc.Shutdown(context.Background())
}

// outageSink fails every Upsert until its outage clears, then succeeds.
type outageSink struct {
	failUntil int64
	attempts  int64
	good      int64
}

func (f *outageSink) Upsert(_ context.Context, _ string, docs []Document) error {
	if atomic.AddInt64(&f.attempts, 1) <= atomic.LoadInt64(&f.failUntil) {
		return fmt.Errorf("backend down")
	}
	atomic.AddInt64(&f.good, int64(len(docs)))
	return nil
}
func (f *outageSink) Delete(context.Context, string, string) error { return nil }

// CHAOS : the backend is down during indexing → every doc is dead-lettered and
// the cursor is NOT advanced ; when the backend recovers, a re-sync re-indexes
// exactly the docs that failed. No data loss, no crash.
func TestService_Chaos_BackendDownThenRecover(t *testing.T) {
	registerLoad()
	svc := NewService(NewMemCursor(), 4)
	var dl int64
	svc.OnDeadLetter(func(SourceSpec, Document, error) { atomic.AddInt64(&dl, 1) })

	spec := SourceSpec{Name: "chaos", Type: "loadfake", KB: "kb", Opts: map[string]any{"docs": 6}}
	sink := &outageSink{failUntil: 1000} // outage : fail every attempt

	ctx := context.Background()
	if _, err := svc.Sync(ctx, spec, sink); err != nil {
		t.Fatalf("sync during outage should not error (isolates): %v", err)
	}
	if g := atomic.LoadInt64(&sink.good); g != 0 {
		t.Fatalf("docs landed despite outage: %d", g)
	}
	if atomic.LoadInt64(&dl) == 0 {
		t.Fatal("no dead-letter recorded during the outage")
	}

	atomic.StoreInt64(&sink.failUntil, 0) // backend recovers
	if _, err := svc.Sync(ctx, spec, sink); err != nil {
		t.Fatalf("re-sync after recovery: %v", err)
	}
	if g := atomic.LoadInt64(&sink.good); g != 6 {
		t.Fatalf("after recovery %d/6 docs re-indexed (cursor must not have advanced for failed docs)", g)
	}
	t.Logf("chaos: outage dead-lettered %d attempts; recovery re-indexed all 6 docs — no loss", atomic.LoadInt64(&dl))
}

// MULTI-NODE : N service instances (simulated nodes) share ONE Postgres cursor
// + advisory lease and all register the SAME source. The lease serialises and
// the shared cursor dedups, so the source is indexed EXACTLY ONCE across the
// cluster (not once per node). Skips without Postgres.
func TestService_MultiNode_ExactlyOnce_Live(t *testing.T) {
	registerLoad()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if p, err := NewPgStore(ctx, pgTestDSN()); err != nil {
		t.Skipf("no Postgres: %v", err)
	} else {
		p.Close()
	}

	const nodes, docs = 3, 20
	sink := &countSink{delay: 3 * time.Millisecond} // the shared backend
	name := fmt.Sprintf("mn-%d", time.Now().UnixNano())
	spec := SourceSpec{
		Name: name, Type: "loadfake", KB: "kb", Owner: "tenant",
		Opts: map[string]any{"docs": docs}, Triggers: []Trigger{{Type: "on_start"}},
	}

	var svcs []*Service
	for i := 0; i < nodes; i++ {
		st, err := NewPgStore(ctx, pgTestDSN()) // own pool, SAME db → shared cursor + lease
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		s := NewService(st, 4)
		svcs = append(svcs, s)
	}
	for _, s := range svcs { // all nodes register the same source at once
		s.Register(spec, sink)
	}

	// Wait for the cluster to settle, then confirm it stays at exactly `docs`.
	deadline := time.Now().Add(8 * time.Second)
	for atomic.LoadInt64(&sink.ups) < docs && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(1 * time.Second) // give any racing node a chance to (wrongly) double-index
	got := atomic.LoadInt64(&sink.ups)
	for _, s := range svcs {
		s.Shutdown(context.Background())
	}
	t.Logf("multi-node: %d nodes sharing one PG lease+cursor → %d upserts (want %d)", nodes, got, docs)
	if got != docs {
		t.Fatalf("NOT exactly-once across nodes: %d upserts, want %d (double-indexing)", got, docs)
	}
}
