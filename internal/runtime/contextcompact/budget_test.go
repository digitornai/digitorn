package contextcompact

import (
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

func msg(seq uint64, role string, chars int) sessionstore.Message {
	return sessionstore.Message{Seq: seq, Role: role, Content: strings.Repeat("x", chars)}
}

func TestSafeSplitIndexBudget_DropsLargeRecentTail(t *testing.T) {
	msgs := []sessionstore.Message{
		msg(1, "user", 80),
		msg(2, "assistant", 4000),
		msg(3, "user", 80),
		msg(4, "assistant", 4000),
		msg(5, "user", 80),
		msg(6, "assistant", 4000),
	}
	countCut := SafeSplitIndex(msgs, 4)
	if countCut != 2 {
		t.Fatalf("count split = %d, want 2 (keep last 4)", countCut)
	}
	budgetCut := SafeSplitIndexBudget(msgs, 4, 1200)
	keptTokens := EstimateTokens(msgs[budgetCut:])
	if keptTokens > 1300 {
		t.Errorf("budget split kept %d tokens, want <= ~1200 (cut=%d)", keptTokens, budgetCut)
	}
	if budgetCut <= countCut {
		t.Errorf("budget split (cut=%d) should drop MORE than count split (cut=%d)", budgetCut, countCut)
	}
}

func TestSafeSplitIndexBudget_KeepsAtLeastOne(t *testing.T) {
	msgs := []sessionstore.Message{msg(1, "user", 100), msg(2, "assistant", 40000)}
	cut := SafeSplitIndexBudget(msgs, 1, 500)
	if cut >= len(msgs) {
		t.Fatalf("cut=%d stranded the whole conversation", cut)
	}
}

func TestSafeSplitIndexBudget_FallsBackToCount(t *testing.T) {
	msgs := []sessionstore.Message{msg(1, "u", 50), msg(2, "a", 50), msg(3, "u", 50), msg(4, "a", 50)}
	if SafeSplitIndexBudget(msgs, 2, 0) != SafeSplitIndex(msgs, 2) {
		t.Error("budget<=0 must equal the count split")
	}
}

func TestSafeSplitIndexBudget_NeverOrphansToolResult(t *testing.T) {
	call := sessionstore.Message{Seq: 2, Role: "assistant", Parts: []sessionstore.MessagePart{
		{Type: sessionstore.PartTypeToolCall, ToolCall: &sessionstore.ToolCallSpec{ID: "tc1"}},
	}}
	result := sessionstore.Message{Seq: 3, Role: "tool", ToolCallIDs: []string{"tc1"}, Parts: []sessionstore.MessagePart{
		{Type: sessionstore.PartTypeToolResult, ToolResult: &sessionstore.ToolResultSpec{ToolCallID: "tc1", Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: strings.Repeat("y", 8000)}}}},
	}}
	msgs := []sessionstore.Message{msg(1, "user", 50), call, result}
	cut := SafeSplitIndexBudget(msgs, 1, 200)
	if hasOrphanToolResult(msgs[cut:]) {
		t.Fatalf("budget split orphaned a tool result (cut=%d)", cut)
	}
}
