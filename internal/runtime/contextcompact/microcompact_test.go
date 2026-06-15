package contextcompact

import (
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

func bigToolResult(seq uint64, callID string, size int) sessionstore.Message {
	body := strings.Repeat("x", size)
	return sessionstore.Message{
		Seq: seq, Role: "tool", ToolCallIDs: []string{callID}, Content: body,
		Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeToolResult, ToolResult: &sessionstore.ToolResultSpec{
			ToolCallID: callID,
			Parts:      []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: body}},
		}}},
	}
}

// TestMicroCompact_ElidesOldBulkyKeepsRecent: old bulky tool results are elided;
// the most recent (kept) one stays full, and pairing is preserved.
func TestMicroCompact_ElidesOldBulkyKeepsRecent(t *testing.T) {
	msgs := []sessionstore.Message{
		bigToolResult(1, "A", 8000),
		msgText(2, "user", "hi"),
		bigToolResult(3, "B", 8000),
		bigToolResult(4, "C", 8000),
	}
	out := MicroCompact(msgs, 1, 4096) // keep the last 1 tool result full

	if !isElidedRef(out[0].Content) {
		t.Error("oldest bulky tool result A not elided")
	}
	if !isElidedRef(out[2].Content) {
		t.Error("bulky tool result B not elided")
	}
	if isElidedRef(out[3].Content) {
		t.Error("most-recent tool result C must stay full")
	}
	if out[0].Parts[0].ToolResult == nil || out[0].Parts[0].ToolResult.ToolCallID != "A" {
		t.Error("elision must preserve the tool_call pairing (ToolCallID)")
	}
}

// TestMicroCompact_KeepsSmall: small tool results are never elided regardless of age.
func TestMicroCompact_KeepsSmall(t *testing.T) {
	msgs := []sessionstore.Message{
		bigToolResult(1, "A", 100), bigToolResult(2, "B", 100), bigToolResult(3, "C", 100),
	}
	out := MicroCompact(msgs, 0, 4096)
	for i := range out {
		if isElidedRef(out[i].Content) {
			t.Errorf("small tool result %d was elided", i)
		}
	}
}

// TestMicroCompact_DoesNotMutateInput: the input messages are never mutated.
func TestMicroCompact_DoesNotMutateInput(t *testing.T) {
	orig := bigToolResult(1, "A", 8000)
	origBody := orig.Content
	msgs := []sessionstore.Message{orig, bigToolResult(2, "B", 100)}
	_ = MicroCompact(msgs, 0, 4096)
	if msgs[0].Content != origBody {
		t.Error("input Content was mutated")
	}
	if msgs[0].Parts[0].ToolResult.Parts[0].Text != origBody {
		t.Error("input ToolResult parts were mutated")
	}
}

// TestMicroCompact_PreservesError: an errored tool result keeps its error signal
// after elision (the agent must still know the call failed).
func TestMicroCompact_PreservesError(t *testing.T) {
	m := bigToolResult(1, "A", 8000)
	m.Parts[0].ToolResult.Error = "boom"
	out := MicroCompact([]sessionstore.Message{m, bigToolResult(2, "B", 100)}, 0, 4096)
	if out[0].Parts[0].ToolResult.Error != "boom" {
		t.Error("error signal lost on elision")
	}
	if out[0].Parts[0].ToolResult.ToolCallID != "A" {
		t.Error("ToolCallID lost on elision")
	}
}

// TestMicroCompact_IgnoresNonToolMessages: big user/assistant messages are never
// touched — only tool results are elidable.
func TestMicroCompact_IgnoresNonToolMessages(t *testing.T) {
	big := strings.Repeat("y", 8000)
	msgs := []sessionstore.Message{msgText(1, "user", big), msgText(2, "assistant", big)}
	out := MicroCompact(msgs, 0, 4096)
	for i := range out {
		if isElidedRef(out[i].Content) {
			t.Errorf("non-tool message %d was elided", i)
		}
	}
}

// TestMicroCompact_NoToolResultsIsNoOp: with no tool results, the same slice is
// returned untouched.
func TestMicroCompact_NoToolResultsIsNoOp(t *testing.T) {
	msgs := []sessionstore.Message{msgText(1, "user", "a"), msgText(2, "assistant", "b")}
	out := MicroCompact(msgs, 1, 4096)
	if len(out) != len(msgs) {
		t.Fatal("length changed")
	}
}
