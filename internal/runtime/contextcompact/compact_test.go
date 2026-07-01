package contextcompact

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// --- builders -------------------------------------------------------

func userMsg(seq uint64, text string) sessionstore.Message {
	return sessionstore.Message{Seq: seq, Role: "user", Content: text,
		Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: text}}}
}

func asstMsg(seq uint64, text string) sessionstore.Message {
	return sessionstore.Message{Seq: seq, Role: "assistant", Content: text,
		Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: text}}}
}

// asstCall is an assistant message that issues a tool call.
func asstCall(seq uint64, callID, name string) sessionstore.Message {
	return sessionstore.Message{Seq: seq, Role: "assistant",
		Parts: []sessionstore.MessagePart{{
			Type:     sessionstore.PartTypeToolCall,
			ToolCall: &sessionstore.ToolCallSpec{ID: callID, Name: name},
		}}}
}

// toolResult is a "tool" message carrying the result of callID.
func toolResult(seq uint64, callID, text string) sessionstore.Message {
	return sessionstore.Message{Seq: seq, Role: "tool", ToolCallIDs: []string{callID},
		Parts: []sessionstore.MessagePart{{
			Type: sessionstore.PartTypeToolResult,
			ToolResult: &sessionstore.ToolResultSpec{
				ToolCallID: callID,
				Parts:      []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: text}},
			},
		}}}
}

// =====================================================================
// SafeSplitIndex
// =====================================================================

func TestSafeSplit_NothingToDropWhenShort(t *testing.T) {
	msgs := []sessionstore.Message{userMsg(1, "a"), asstMsg(2, "b")}
	if got := SafeSplitIndex(msgs, 10); got != 0 {
		t.Errorf("short history must not split, got %d", got)
	}
}

func TestSafeSplit_PlainKeepRecent(t *testing.T) {
	var msgs []sessionstore.Message
	for i := uint64(1); i <= 10; i++ {
		msgs = append(msgs, userMsg(i, "m"))
	}
	// keepRecent=3 → cut at 7, no tool pairs to worry about.
	if got := SafeSplitIndex(msgs, 3); got != 7 {
		t.Errorf("cut = %d, want 7", got)
	}
}

func TestSafeSplit_NeverStrandsToolResult(t *testing.T) {
	// Layout (idx): 0 user, 1 asst-call(c1), 2 tool-result(c1), 3 user,
	// 4 asst, 5 user. keepRecent=4 → naive cut=2 would start the kept
	// slice at the tool result (idx2) whose call is at idx1 (dropped) →
	// orphan. Must pull back to idx1 (include the call).
	msgs := []sessionstore.Message{
		userMsg(1, "do X"),
		asstCall(2, "c1", "filesystem.read"),
		toolResult(3, "c1", "file contents"),
		userMsg(4, "thanks"),
		asstMsg(5, "ok"),
		userMsg(6, "next"),
	}
	cut := SafeSplitIndex(msgs, 4)
	if cut > 1 {
		t.Fatalf("cut = %d, must be <=1 to keep the c1 call with its result", cut)
	}
	if hasOrphanToolResult(msgs[cut:]) {
		t.Errorf("kept slice still has an orphan tool result at cut=%d", cut)
	}
}

func TestSafeSplit_PullsBackPastToolBoundary(t *testing.T) {
	// The kept boundary lands exactly on a tool result → must move back
	// to include its assistant call.
	msgs := []sessionstore.Message{
		userMsg(1, "u1"),
		userMsg(2, "u2"),
		asstCall(3, "cA", "web.fetch"),
		toolResult(4, "cA", "html"),
	}
	// keepRecent=1 → naive cut=3 (keep only the tool result) → orphan →
	// pull back to 2 (include the call).
	cut := SafeSplitIndex(msgs, 1)
	if hasOrphanToolResult(msgs[cut:]) {
		t.Errorf("orphan remained at cut=%d", cut)
	}
	if cut != 2 {
		t.Errorf("cut = %d, want 2 (keep cA call + its result)", cut)
	}
}

func TestSafeSplit_KeepRecentZeroDefaultsToOne(t *testing.T) {
	msgs := []sessionstore.Message{userMsg(1, "a"), userMsg(2, "b"), userMsg(3, "c")}
	if got := SafeSplitIndex(msgs, 0); got != 2 {
		t.Errorf("keepRecent<=0 must keep 1 → cut=2, got %d", got)
	}
}

// =====================================================================
// Truncate
// =====================================================================

func TestTruncate_DropsAndInjectsRecap(t *testing.T) {
	var msgs []sessionstore.Message
	for i := uint64(1); i <= 12; i++ {
		msgs = append(msgs, userMsg(i, "message body here"))
	}
	res := Truncate(msgs, 4, "build the thing")
	if res.Dropped != 8 {
		t.Fatalf("dropped = %d, want 8", res.Dropped)
	}
	if res.Strategy != StrategyTruncate {
		t.Errorf("strategy = %q", res.Strategy)
	}
	if res.CutoffSeq != 8 {
		t.Errorf("cutoff = %d, want 8 (seq of last dropped)", res.CutoffSeq)
	}
	// First message must be the injected system recap.
	if res.Messages[0].Role != "system" {
		t.Fatalf("first message role = %q, want system", res.Messages[0].Role)
	}
	if !strings.Contains(res.Messages[0].Content, "build the thing") {
		t.Errorf("recap missing goal: %q", res.Messages[0].Content)
	}
	// Kept slice = recap + last 4.
	if len(res.Messages) != 5 {
		t.Errorf("len = %d, want 5 (recap + 4 kept)", len(res.Messages))
	}
	if res.Messages[len(res.Messages)-1].Seq != 12 {
		t.Errorf("last kept seq = %d, want 12", res.Messages[len(res.Messages)-1].Seq)
	}
}

func TestTruncate_NoOpWhenShort(t *testing.T) {
	msgs := []sessionstore.Message{userMsg(1, "a"), userMsg(2, "b")}
	res := Truncate(msgs, 10, "goal")
	if res.Dropped != 0 || len(res.Messages) != 2 {
		t.Errorf("short history must be a no-op, got dropped=%d len=%d", res.Dropped, len(res.Messages))
	}
}

// =====================================================================
// Summarize + fallback
// =====================================================================

type stubSummarizer struct {
	out  string
	err  error
	seen int
}

func (s *stubSummarizer) Summarize(_ context.Context, dropped []sessionstore.Message, _ int) (string, error) {
	s.seen = len(dropped)
	return s.out, s.err
}

func TestSummarize_UsesSummaryBrain(t *testing.T) {
	var msgs []sessionstore.Message
	for i := uint64(1); i <= 12; i++ {
		msgs = append(msgs, userMsg(i, "body"))
	}
	s := &stubSummarizer{out: "the agent did A, B, C"}
	res := Summarize(context.Background(), msgs, 4, s, 1024, "goal", "")
	if res.Strategy != StrategySummarize {
		t.Fatalf("strategy = %q, want summarize", res.Strategy)
	}
	if s.seen != 8 {
		t.Errorf("summarizer saw %d dropped, want 8", s.seen)
	}
	if res.Messages[0].Role != "system" || !strings.Contains(res.Messages[0].Content, "did A, B, C") {
		t.Errorf("summary not injected: %q", res.Messages[0].Content)
	}
}

func TestSummarize_FallsBackToTruncateOnError(t *testing.T) {
	var msgs []sessionstore.Message
	for i := uint64(1); i <= 12; i++ {
		msgs = append(msgs, userMsg(i, "body"))
	}
	s := &stubSummarizer{err: errors.New("summary brain down")}
	res := Summarize(context.Background(), msgs, 4, s, 1024, "goal", "")
	if res.Strategy != StrategyTruncate {
		t.Errorf("must fall back to truncate on summary error, got %q", res.Strategy)
	}
	if res.Dropped != 8 {
		t.Errorf("fallback must still drop, got %d", res.Dropped)
	}
}

func TestSummarize_FallsBackOnEmptySummary(t *testing.T) {
	var msgs []sessionstore.Message
	for i := uint64(1); i <= 12; i++ {
		msgs = append(msgs, userMsg(i, "body"))
	}
	s := &stubSummarizer{out: "   "}
	res := Summarize(context.Background(), msgs, 4, s, 1024, "goal", "")
	if res.Strategy != StrategyTruncate {
		t.Errorf("empty summary must fall back to truncate, got %q", res.Strategy)
	}
}

func TestSummarize_NilSummarizerFallsBack(t *testing.T) {
	var msgs []sessionstore.Message
	for i := uint64(1); i <= 8; i++ {
		msgs = append(msgs, userMsg(i, "body"))
	}
	res := Summarize(context.Background(), msgs, 3, nil, 1024, "goal", "")
	if res.Strategy != StrategyTruncate || res.Dropped == 0 {
		t.Errorf("nil summarizer must truncate, got strategy=%q dropped=%d", res.Strategy, res.Dropped)
	}
}

func TestSummarize_NoOpKeepsSummarizeStrategy(t *testing.T) {
	msgs := []sessionstore.Message{userMsg(1, "a")}
	s := &stubSummarizer{out: "x"}
	res := Summarize(context.Background(), msgs, 10, s, 1024, "goal", "")
	if res.Dropped != 0 {
		t.Errorf("no-op expected, dropped=%d", res.Dropped)
	}
	if s.seen != 0 {
		t.Errorf("summarizer must not be called on a no-op")
	}
}

// =====================================================================
// EstimateTokens
// =====================================================================

func TestEstimateTokens_Grows(t *testing.T) {
	small := []sessionstore.Message{userMsg(1, "hi")}
	big := []sessionstore.Message{userMsg(1, strings.Repeat("x", 4000))}
	if EstimateTokens(big) <= EstimateTokens(small) {
		t.Error("bigger content must estimate more tokens")
	}
	// ~4 chars/token : 4000 chars ≈ 1000 tokens (plus framing).
	if got := EstimateTokens(big); got < 900 || got > 1200 {
		t.Errorf("token estimate %d out of expected band for 4000 chars", got)
	}
}

// =====================================================================
// Tool-pair integrity end-to-end : after compaction the kept view must
// still be a valid conversation (no orphan results).
// =====================================================================

func TestCompactedViewAlwaysValid(t *testing.T) {
	// Interleave tool-call/result pairs heavily, then compact at several
	// keepRecent values ; the kept slice must NEVER have an orphan.
	var msgs []sessionstore.Message
	seq := uint64(0)
	for i := 0; i < 20; i++ {
		seq++
		msgs = append(msgs, userMsg(seq, "ask"))
		seq++
		cid := "call" + string(rune('A'+i))
		msgs = append(msgs, asstCall(seq, cid, "filesystem.read"))
		seq++
		msgs = append(msgs, toolResult(seq, cid, "result"))
	}
	for _, keep := range []int{1, 2, 5, 10, 15, 30} {
		res := Truncate(msgs, keep, "goal")
		// Drop the injected system recap (index 0) before checking pairs.
		view := res.Messages
		if res.Dropped > 0 {
			view = res.Messages[1:]
		}
		if hasOrphanToolResult(view) {
			t.Errorf("keepRecent=%d produced an orphan tool result in the compacted view", keep)
		}
	}
}
