package indexer

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// flakySink simulates a vector backend that is DOWN for a window then RECOVERS.
// While down, every Upsert fails (both batch and per-doc isolation), so all docs
// dead-letter and their cursor hashes are NOT advanced. After recovery, the next
// sync must retry the dead-lettered docs and land them — no data loss, no need
// to re-emit from the source.
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

// TestChaos_BackendDownMidIndex_DeadLetterThenRetry proves the headline
// resilience contract: with the backend DOWN, a sync dead-letters every doc and
// advances NO cursor hash; once the backend RECOVERS, the very next sync (same
// source emitting the same docs) lands all of them exactly once. No data loss
// across the outage; no duplicate once recovered.
func TestChaos_BackendDownMidIndex_DeadLetterThenRetry(t *testing.T) {
	registerLoad()
	cur := NewMemCursor()
	svc := NewService(cur, 4)
	var dl int
	svc.OnDeadLetter(func(SourceSpec, Document, error) { dl++ })

	spec := SourceSpec{Name: "src", Type: "loadfake", KB: "kb", Opts: map[string]any{"docs": 6}}
	sink := newFlakySink() // starts DOWN

	// Sync 1: backend down -> all 6 dead-lettered, nothing landed, no hash saved.
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

	// Backend recovers.
	sink.setUp()

	// Sync 2: same source, same docs. Because no hash was saved for the dead-
	// lettered docs, they are ALL re-sent and now land. Recovery with no loss.
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

	// Sync 3: nothing changed and all hashes now saved -> zero re-sends.
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
