package sessionstore

import (
	"context"
	"fmt"
	"testing"
)

func TestCTXLoad_Lifecycle_WindowBounded_TranscriptLossless(t *testing.T) {
	paths := NewPaths(t.TempDir())
	sid := "ctxload"
	state := NewSessionState(sid)

	var seq uint64
	for i := 1; i <= 10; i++ {
		role := "user"
		if i%2 == 0 {
			role = "assistant"
		}
		seq++
		ev := txMsgEvent(seq, sid, role, fmt.Sprintf("MSG-%d", i))
		writeEvents(t, paths, sid, ev)
		Apply(state, &ev)
	}

	seq++
	cc := Event{
		Seq: seq, SessionID: sid, Type: EventContextCompacted,
		CtxCompact: &ContextCompactPayload{CutoffSeq: 6, Summary: "recap of MSG-1..6", Strategy: "truncate"},
	}
	writeEvents(t, paths, sid, cc)
	Apply(state, &cc)

	if got := len(state.Messages); got != 4 {
		t.Fatalf("in-memory window after context compaction = %d, want 4 (post-cutoff 6)", got)
	}

	c := NewCompactor(CompactorConfig{Paths: paths})
	if _, err := c.Compact(context.Background(), state, CompactOptions{TruncateMode: TruncateSync}); err != nil {
		t.Fatalf("storage compact: %v", err)
	}

	res, err := Load(paths, sid, LoadOptions{Mode: JSONLBestEffort})
	if err != nil {
		t.Fatalf("cold load: %v", err)
	}
	cold := res.State

	if got := len(cold.Messages); got != 4 {
		t.Fatalf("cold-loaded window = %d, want 4 (anchored at context cutoff 6)", got)
	}
	for _, m := range cold.Messages {
		if m.Seq != 0 && m.Seq <= 6 {
			t.Errorf("cold window holds pre-cutoff message seq %d — not anchored at the compaction", m.Seq)
		}
	}

	full, err := ReadTranscript(paths, sid)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if len(full) != 10 {
		t.Fatalf("durable transcript lost messages: got %d, want 10 — DATA LOSS", len(full))
	}
	for i := 1; i <= 10; i++ {
		want := fmt.Sprintf("MSG-%d", i)
		found := false
		for j := range full {
			if full[j].Content == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("durable transcript missing %s — DATA LOSS through the compaction lifecycle", want)
		}
	}
}
