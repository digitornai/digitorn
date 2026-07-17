package sessionstore

import "testing"

func TestProjection_AbortedCompaction_ClearsFlagOnly(t *testing.T) {
	s := NewSessionState("s1")
	Apply(s, &Event{Seq: 1, Type: EventTokenUsage, SessionID: "s1",
		Cost: &CostPayload{TokensIn: 4000, TokensOut: 1000}})
	Apply(s, &Event{Seq: 2, Type: EventContextCompacted, SessionID: "s1",
		CtxCompact: &ContextCompactPayload{CutoffSeq: 1, Summary: "earlier", Strategy: "summarize", MessagesDropped: 1}})
	if s.ContextProviderTokens != 0 {
		t.Fatalf("a REAL compaction must invalidate the provider anchor, got %d", s.ContextProviderTokens)
	}
	Apply(s, &Event{Seq: 3, Type: EventTokenUsage, SessionID: "s1",
		Cost: &CostPayload{TokensIn: 1800, TokensOut: 200}})
	prior := *s.ContextCompaction
	anchorBefore := s.ContextProviderTokens
	if anchorBefore != 2000 {
		t.Fatalf("anchor = %d, want 2000", anchorBefore)
	}

	Apply(s, &Event{Seq: 4, Type: EventContextCompacting, SessionID: "s1",
		CtxCompact: &ContextCompactPayload{CutoffSeq: 3, Strategy: "summarize", MessagesDropped: 2}})
	if !s.CompactionInflight {
		t.Fatal("START must raise CompactionInflight")
	}
	Apply(s, &Event{Seq: 5, Type: EventContextCompacted, SessionID: "s1",
		CtxCompact: &ContextCompactPayload{CutoffSeq: 0, Strategy: "aborted", MessagesDropped: 0}})

	if s.CompactionInflight {
		t.Fatal("aborted END must clear CompactionInflight")
	}
	if s.ContextCompaction == nil || s.ContextCompaction.CutoffSeq != prior.CutoffSeq || s.ContextCompaction.Summary != prior.Summary {
		t.Fatalf("aborted compaction must NOT change the view, got %+v want %+v", s.ContextCompaction, prior)
	}
	if s.ContextProviderTokens != anchorBefore {
		t.Fatalf("aborted compaction must NOT invalidate the anchor, got %d want %d", s.ContextProviderTokens, anchorBefore)
	}
}
