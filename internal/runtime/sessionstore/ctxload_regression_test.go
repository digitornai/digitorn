package sessionstore

import (
	"context"
	"fmt"
	"testing"
)

// TestCTXLoad_Lifecycle_WindowBounded_TranscriptLossless is the keystone
// guarantee of the bounded-context-load primitive : through the FULL lifecycle
// — context compaction, then storage compaction (snapshot + JSONL truncate),
// then a cold reload — the model's in-memory window stays bounded to the last
// compaction while the durable transcript loses NOTHING. A regression here is a
// silent data-loss bug.
func TestCTXLoad_Lifecycle_WindowBounded_TranscriptLossless(t *testing.T) {
	paths := NewPaths(t.TempDir())
	sid := "ctxload"
	state := NewSessionState(sid)

	// 1. Seed 10 messages durably (write to JSONL + project), mirroring
	//    AppendDurable's fsync-then-project order.
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

	// 2. Context-compact at cutoff seq 6 (keep 7..10). Durable event + projection
	//    trims the in-memory window.
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

	// 3. Storage-compact : lossless snapshot + JSONL truncate.
	c := NewCompactor(CompactorConfig{Paths: paths})
	if _, err := c.Compact(context.Background(), state, CompactOptions{TruncateMode: TruncateSync}); err != nil {
		t.Fatalf("storage compact: %v", err)
	}

	// 4. Cold reload — a fresh process re-hydrating from disk.
	res, err := Load(paths, sid, LoadOptions{Mode: JSONLBestEffort})
	if err != nil {
		t.Fatalf("cold load: %v", err)
	}
	cold := res.State

	// The reloaded window is bounded, anchored at the context cutoff : only the
	// post-cutoff messages, never the pre-cutoff ones.
	if got := len(cold.Messages); got != 4 {
		t.Fatalf("cold-loaded window = %d, want 4 (anchored at context cutoff 6)", got)
	}
	for _, m := range cold.Messages {
		if m.Seq != 0 && m.Seq <= 6 {
			t.Errorf("cold window holds pre-cutoff message seq %d — not anchored at the compaction", m.Seq)
		}
	}

	// 5. THE GUARANTEE : every one of the 10 messages survives in the durable
	//    transcript through context-compact + storage-compact + reload.
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
