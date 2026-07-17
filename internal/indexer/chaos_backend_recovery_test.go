package indexer

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

type flakySink struct {
	mu      sync.Mutex
	down    bool
	landed  map[string]int
}

func newFlakySink() *flakySink { return &flakySink{down: true, landed: map[string]int{}} }

func (s *flakySink) setUp() { s.mu.Lock(); s.down = false; s.mu.Unlock() }

func (s *flakySink) Upsert(_ context.Context, _ string, docs []Document) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return fmt.Errorf("backend down")
	}
	for _, d := range docs {
		s.landed[d.ID]++
	}
	return nil
}
func (s *flakySink) Delete(context.Context, string, string) error { return nil }
func (s *flakySink) landedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.landed)
}
func (s *flakySink) dupes() (string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, n := range s.landed {
		if n > 1 {
			return id, n
		}
	}
	return "", 0
}

func TestChaos_BackendDownMidIndex_DeadLetterThenRetry(t *testing.T) {
	registerLoad()
	cur := NewMemCursor()
	svc := NewService(cur, 4)
	var dl int
	svc.OnDeadLetter(func(SourceSpec, Document, error) { dl++ })

	spec := SourceSpec{Name: "src", Type: "loadfake", KB: "kb", Opts: map[string]any{"docs": 6}}
	sink := newFlakySink()

	rep1, err := svc.Sync(context.Background(), spec, sink)
	if err != nil {
		t.Fatalf("sync1 returned error (should isolate): %v", err)
	}
	if sink.landedCount() != 0 {
		t.Fatalf("sync1: %d docs landed despite backend down", sink.landedCount())
	}
	if st := svc.Stats(); st.DeadLettered != 6 {
		t.Fatalf("sync1: DeadLettered=%d, want 6", st.DeadLettered)
	}
	t.Logf("sync1 (backend DOWN): landed=%d deadLettered=%d rep=%+v", sink.landedCount(), dl, rep1)

	sink.setUp()

	rep2, err := svc.Sync(context.Background(), spec, sink)
	if err != nil {
		t.Fatalf("sync2: %v", err)
	}
	if sink.landedCount() != 6 {
		t.Fatalf("RECOVERY DATA LOSS: only %d/6 docs landed after backend recovery", sink.landedCount())
	}
	if id, n := sink.dupes(); n > 0 {
		t.Fatalf("RECOVERY DUP: doc %s landed %d times after retry", id, n)
	}
	t.Logf("sync2 (backend UP): landed=%d/6 rep=%+v — full recovery, no loss, no dup", sink.landedCount(), rep2)

	before := sink.landedCount()
	if _, err := svc.Sync(context.Background(), spec, sink); err != nil {
		t.Fatal(err)
	}
	for id, n := range sink.landed {
		if n != 1 {
			t.Fatalf("sync3: doc %s re-sent (count=%d) despite unchanged + already-saved hash", id, n)
		}
	}
	t.Logf("sync3 (steady state): still %d docs, each landed exactly once (idempotent).", before)
	t.Log("CONFIRMED: backend-down-mid-index → dead-letter (no hash advance) → recover → next sync retries & lands all, exactly once. Resilience contract holds.")
}
