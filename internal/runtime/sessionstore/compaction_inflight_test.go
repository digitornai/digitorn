package sessionstore

import "testing"

// TestProjection_CompactionInflightPairing proves the start/end markers drive
// the projected CompactionInflight flag : the START marker raises it, the END
// marker clears it and records the durable view cutoff. This is what lets a
// client that loads the state snapshot (rather than replaying the stream) know
// a compaction is happening right now.
func TestProjection_CompactionInflightPairing(t *testing.T) {
	s := NewSessionState("s1")

	if s.CompactionInflight {
		t.Fatal("fresh session must not be flagged inflight")
	}

	Apply(s, &Event{
		Seq: 10, Type: EventContextCompacting, SessionID: "s1",
		CtxCompact: &ContextCompactPayload{CutoffSeq: 6, Strategy: "summarize", MessagesDropped: 6},
	})
	if !s.CompactionInflight {
		t.Fatal("START marker must raise CompactionInflight")
	}
	if s.ContextCompaction != nil {
		t.Fatal("START marker must NOT change the view — the cutoff only applies on END")
	}

	Apply(s, &Event{
		Seq: 11, Type: EventContextCompacted, SessionID: "s1",
		CtxCompact: &ContextCompactPayload{CutoffSeq: 6, Summary: "recap", Strategy: "summarize", MessagesDropped: 6},
	})
	if s.CompactionInflight {
		t.Fatal("END marker must clear CompactionInflight")
	}
	if s.ContextCompaction == nil || s.ContextCompaction.CutoffSeq != 6 {
		t.Fatalf("END marker must record the view cutoff, got %+v", s.ContextCompaction)
	}

	// The flag survives a snapshot round-trip while inflight.
	s2 := NewSessionState("s2")
	Apply(s2, &Event{Seq: 5, Type: EventContextCompacting, SessionID: "s2",
		CtxCompact: &ContextCompactPayload{CutoffSeq: 2, Strategy: "truncate", MessagesDropped: 2}})
	if !s2.Snapshot().CompactionInflight {
		t.Fatal("CompactionInflight must be carried into the snapshot")
	}
}
