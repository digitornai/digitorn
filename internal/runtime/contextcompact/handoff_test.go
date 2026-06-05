package contextcompact

import (
	"context"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// The compaction handoff must read as an authoritative runtime checkpoint, not
// as conversational text, and must carry the "continue as if no compaction
// happened" contract — on BOTH the summarize path and the truncate floor.

func twelveMsgs() []sessionstore.Message {
	var msgs []sessionstore.Message
	for i := uint64(1); i <= 12; i++ {
		msgs = append(msgs, userMsg(i, "body"))
	}
	return msgs
}

func assertFramed(t *testing.T, body string) {
	t.Helper()
	for _, want := range []string{"Context checkpoint", "<recap>", "</recap>", "as if"} {
		if !strings.Contains(body, want) {
			t.Errorf("handoff missing %q in:\n%s", want, body)
		}
	}
}

func TestHandoff_SummarizeIsFramed(t *testing.T) {
	s := &stubSummarizer{out: "MISSION: ship X\nTASK & PLAN: do A then B"}
	res := Summarize(context.Background(), twelveMsgs(), 4, s, 2048, "ship X", "")
	if res.Strategy != StrategySummarize {
		t.Fatalf("strategy = %q, want summarize", res.Strategy)
	}
	body := res.Messages[0].Content
	assertFramed(t, body)
	// The LLM recap must sit INSIDE the <recap> envelope, verbatim.
	if !strings.Contains(body, "MISSION: ship X") {
		t.Errorf("LLM summary not carried in handoff:\n%s", body)
	}
	open := strings.Index(body, "<recap>")
	close := strings.Index(body, "</recap>")
	if open < 0 || close < 0 || strings.Index(body, "MISSION: ship X") < open || strings.Index(body, "MISSION: ship X") > close {
		t.Errorf("recap not enclosed by <recap>…</recap>:\n%s", body)
	}
	// res.Summary (persisted in the durable marker) equals the framed body so a
	// resume re-injects the SAME handoff, not the bare LLM text.
	if res.Summary != body {
		t.Errorf("Summary marker must equal the framed body")
	}
}

func TestHandoff_TruncateFloorIsFramed(t *testing.T) {
	// No LLM: the deterministic floor must still hand off with the same framing.
	res := Truncate(twelveMsgs(), 4, "ship X")
	if res.Strategy != StrategyTruncate {
		t.Fatalf("strategy = %q, want truncate", res.Strategy)
	}
	body := res.Messages[0].Content
	assertFramed(t, body)
	if !strings.Contains(body, "ship X") {
		t.Errorf("truncate handoff missing goal:\n%s", body)
	}
}

func TestHandoff_SummarizeFallbackStillFramed(t *testing.T) {
	// Summary brain returns empty → falls back to truncate, which is also framed.
	s := &stubSummarizer{out: "  "}
	res := Summarize(context.Background(), twelveMsgs(), 4, s, 2048, "ship X", "")
	if res.Strategy != StrategyTruncate {
		t.Fatalf("must fall back to truncate, got %q", res.Strategy)
	}
	assertFramed(t, res.Messages[0].Content)
}

func TestHandoff_PriorSummaryFedForCumulativeMerge(t *testing.T) {
	// A prior recap must be fed to the summarizer as leading context so the new
	// handoff MERGES it (cumulative) instead of forgetting pre-window history.
	s := &stubSummarizer{out: "merged recap"}
	prior := "PRIOR: earlier mission state"
	_ = Summarize(context.Background(), twelveMsgs(), 4, s, 2048, "ship X", prior)
	// 8 dropped + 1 synthetic prior-summary message = 9 seen by the summarizer.
	if s.seen != 9 {
		t.Errorf("summarizer saw %d messages, want 9 (8 dropped + prior recap)", s.seen)
	}
}
