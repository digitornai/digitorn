package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// llmSeenText concatenates all text the LLM received across its messages
// (Content + text parts) so a test can assert what the model did / did
// not see in its prompt.
func llmSeenText(req *llm.ChatRequest) string {
	if req == nil {
		return ""
	}
	var b strings.Builder
	for _, m := range req.Messages {
		b.WriteString(m.Role)
		b.WriteByte(':')
		b.WriteString(m.Content)
		for _, p := range m.Parts {
			if p.Text != "" {
				b.WriteByte(' ')
				b.WriteString(p.Text)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// TestCX3_EngineAppliesContextCompactionView : with a durable context
// compaction marker, the model must see the COMPACTED VIEW — the summary
// system message + the messages after the cutoff — and NOT the dropped
// older messages. The on-disk history keeps everything.
func TestCX3_EngineAppliesContextCompactionView(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-cx")
	ctx := context.Background()

	// Seed history : two old exchanges + one recent message.
	appendUserMsg(t, sess, "sess-cx", "OLD-MESSAGE-1") // seq 1
	_, _ = sess.AppendDurable(ctx, sessionstore.Event{
		Type: sessionstore.EventAssistantMessage, SessionID: "sess-cx",
		Message: &sessionstore.MessagePayload{Role: "assistant", Content: "OLD-REPLY-1"},
	}) // seq 2
	appendUserMsg(t, sess, "sess-cx", "OLD-MESSAGE-2")  // seq 3
	appendUserMsg(t, sess, "sess-cx", "RECENT-KEEP-ME") // seq 4

	// Durable compaction marker : drop seq<=3, inject the summary.
	_, _ = sess.AppendDurable(ctx, sessionstore.Event{
		Type: sessionstore.EventContextCompacted, SessionID: "sess-cx",
		CtxCompact: &sessionstore.ContextCompactPayload{
			CutoffSeq: 3, Summary: "SUMMARY-OF-OLD-STUFF", KeepRecent: 1, Strategy: "summarize",
		},
	}) // seq 5

	lc := &stubLLM{resp: &llm.ChatResponse{Content: "done"}}
	e := newEngine(t, apps, sess, lc)
	if _, err := e.Run(ctx, runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-cx", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	seen := llmSeenText(lc.got)

	// The model MUST see the summary + the recent message.
	if !strings.Contains(seen, "SUMMARY-OF-OLD-STUFF") {
		t.Errorf("LLM did not receive the compaction summary.\nSaw:\n%s", seen)
	}
	if !strings.Contains(seen, "RECENT-KEEP-ME") {
		t.Errorf("LLM did not receive the recent (kept) message.\nSaw:\n%s", seen)
	}
	// The model MUST NOT see the dropped older messages.
	for _, dropped := range []string{"OLD-MESSAGE-1", "OLD-REPLY-1", "OLD-MESSAGE-2"} {
		if strings.Contains(seen, dropped) {
			t.Errorf("LLM saw dropped message %q — compaction view not applied.\nSaw:\n%s", dropped, seen)
		}
	}

	// The durable history MUST still hold everything (audit). The in-memory
	// window is now bounded to the model's view (CTXLOAD), so the full transcript
	// lives in the durable event log — verify OLD-MESSAGE-1 survives there.
	st, _ := sess.State("sess-cx")
	full := st.Snapshot()
	sess.mu.Lock()
	durable := sessionstore.TranscriptFromParts(nil, append([]sessionstore.Event(nil), sess.events...))
	sess.mu.Unlock()
	var foundOld bool
	for _, m := range durable {
		if m.Content == "OLD-MESSAGE-1" {
			foundOld = true
		}
	}
	if !foundOld {
		t.Error("compaction destroyed history — OLD-MESSAGE-1 missing from the durable transcript (must be preserved)")
	}
	if full.ContextCompaction == nil || full.ContextCompaction.CutoffSeq != 3 {
		t.Errorf("projected ContextCompaction marker wrong: %+v", full.ContextCompaction)
	}
}

// TestCX3_NoMarkerNoChange : without a compaction marker the model sees
// the full history (the apply step is a clean no-op).
func TestCX3_NoMarkerNoChange(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-cx2")
	ctx := context.Background()

	appendUserMsg(t, sess, "sess-cx2", "KEEP-ALL-1")
	appendUserMsg(t, sess, "sess-cx2", "KEEP-ALL-2")

	lc := &stubLLM{resp: &llm.ChatResponse{Content: "done"}}
	e := newEngine(t, apps, sess, lc)
	if _, err := e.Run(ctx, runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-cx2", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	seen := llmSeenText(lc.got)
	if !strings.Contains(seen, "KEEP-ALL-1") || !strings.Contains(seen, "KEEP-ALL-2") {
		t.Errorf("no-marker turn must show full history.\nSaw:\n%s", seen)
	}
}
