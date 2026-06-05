package sessionstore

import "testing"

// Abort cancels EVERYTHING, including an in-flight compaction. When the user
// aborts mid-compaction the daemon emits an END marker with CutoffSeq 0 ("the
// compaction was abandoned, nothing applied"). The projection must then ONLY
// clear the in-flight flag : no cutoff, no provider-anchor invalidation, no
// regression of any prior compaction — the context is left exactly as it was.
func TestProjection_AbortedCompaction_ClearsFlagOnly(t *testing.T) {
	s := NewSessionState("s1")
	// Establish a provider anchor (the last turn's exact provider count) and a
	// prior real compaction, so we can prove neither is disturbed by an abort.
	Apply(s, &Event{Seq: 1, Type: EventTokenUsage, SessionID: "s1",
		Cost: &CostPayload{TokensIn: 4000, TokensOut: 1000}}) // anchor = 5000
	Apply(s, &Event{Seq: 2, Type: EventContextCompacted, SessionID: "s1",
		CtxCompact: &ContextCompactPayload{CutoffSeq: 1, Summary: "earlier", Strategy: "summarize", MessagesDropped: 1}})
	if s.ContextProviderTokens != 0 {
		t.Fatalf("a REAL compaction must invalidate the provider anchor, got %d", s.ContextProviderTokens)
	}
	// Re-anchor (a turn ran after that compaction).
	Apply(s, &Event{Seq: 3, Type: EventTokenUsage, SessionID: "s1",
		Cost: &CostPayload{TokensIn: 1800, TokensOut: 200}}) // anchor = 2000
	prior := *s.ContextCompaction
	anchorBefore := s.ContextProviderTokens
	if anchorBefore != 2000 {
		t.Fatalf("anchor = %d, want 2000", anchorBefore)
	}

	// Now: a compaction starts, then the user aborts → START raised, then an
	// aborted END (CutoffSeq 0).
	Apply(s, &Event{Seq: 4, Type: EventContextCompacting, SessionID: "s1",
		CtxCompact: &ContextCompactPayload{CutoffSeq: 3, Strategy: "summarize", MessagesDropped: 2}})
	if !s.CompactionInflight {
		t.Fatal("START must raise CompactionInflight")
	}
	Apply(s, &Event{Seq: 5, Type: EventContextCompacted, SessionID: "s1",
		CtxCompact: &ContextCompactPayload{CutoffSeq: 0, Strategy: "aborted", MessagesDropped: 0}})

	// The flag is cleared (never wedged at "compacting…").
	if s.CompactionInflight {
		t.Fatal("aborted END must clear CompactionInflight")
	}
	// The prior compaction view is untouched (no cutoff applied by the abort).
	if s.ContextCompaction == nil || s.ContextCompaction.CutoffSeq != prior.CutoffSeq || s.ContextCompaction.Summary != prior.Summary {
		t.Fatalf("aborted compaction must NOT change the view, got %+v want %+v", s.ContextCompaction, prior)
	}
	// The provider anchor is untouched (the abort compacted nothing, so the
	// tokenizer calibration anchor stays valid).
	if s.ContextProviderTokens != anchorBefore {
		t.Fatalf("aborted compaction must NOT invalidate the anchor, got %d want %d", s.ContextProviderTokens, anchorBefore)
	}
}
