package indexer

import (
	"context"
	"fmt"
	"testing"
)

// allFailSink fails EVERY upsert (both batch and per-doc isolation) — a fully
// broken sink (e.g. embedder gateway down, vector backend unreachable).
type allFailSink struct{}

func (allFailSink) Upsert(context.Context, string, []Document) error {
	return fmt.Errorf("sink permanently down")
}
func (allFailSink) Delete(context.Context, string, string) error { return nil }

// TestChaos_AllDeadLettered_StillCountsSyncOK proves the documented metric-
// masking gap: when EVERY document in a sync dead-letters (sink fully broken),
// Sync returns nil error and increments SyncsOK — a fully-poisoned source is
// reported as a SUCCESS. Only the DeadLettered counter reveals the failure, so
// an operator alerting on SyncsFailed sees green while indexing 0 documents.
func TestChaos_AllDeadLettered_StillCountsSyncOK(t *testing.T) {
	registerLoad()
	svc := NewService(NewMemCursor(), 2)
	var dl int
	svc.OnDeadLetter(func(SourceSpec, Document, error) { dl++ })

	spec := SourceSpec{Name: "src", Type: "loadfake", KB: "kb", Opts: map[string]any{"docs": 5}}
	rep, err := svc.Sync(context.Background(), spec, allFailSink{})

	if err != nil {
		t.Fatalf("Sync returned an error (it actually swallows it): %v", err)
	}
	st := svc.Stats()
	t.Logf("rep=%+v stats: SyncsOK=%d SyncsFailed=%d DeadLettered=%d DocsUpserted=%d",
		rep, st.SyncsOK, st.SyncsFailed, st.DeadLettered, st.DocsUpserted)

	if st.SyncsOK != 1 {
		t.Fatalf("expected SyncsOK=1 (the masking bug), got %d", st.SyncsOK)
	}
	if st.SyncsFailed != 0 {
		t.Fatalf("expected SyncsFailed=0 (failure is masked), got %d", st.SyncsFailed)
	}
	if st.DeadLettered != 5 || dl != 5 {
		t.Fatalf("expected all 5 docs dead-lettered, got counter=%d hook=%d", st.DeadLettered, dl)
	}
	if st.DocsUpserted != 0 {
		t.Fatalf("expected 0 docs upserted, got %d", st.DocsUpserted)
	}
	t.Log("CONFIRMED metric-masking: a fully-failed sync (5/5 dead-lettered, 0 upserted) reports SyncsOK=1, SyncsFailed=0. Alerting on SyncsFailed/SyncsOK ratio is blind to a totally broken sink.")
}
