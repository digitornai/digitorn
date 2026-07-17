package contextcompact

import (
	"testing"

	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

func msgText(seq uint64, role, text string) sessionstore.Message {
	return sessionstore.Message{
		Seq:  seq,
		Role: role,
		Parts: []sessionstore.MessagePart{
			{Type: sessionstore.PartTypeText, Text: text},
		},
	}
}

func msgToolCall(seq uint64, callID string) sessionstore.Message {
	return sessionstore.Message{
		Seq:  seq,
		Role: "assistant",
		Parts: []sessionstore.MessagePart{
			{Type: sessionstore.PartTypeToolCall, ToolCall: &sessionstore.ToolCallSpec{ID: callID}},
		},
	}
}

func msgToolResult(seq uint64, callID string) sessionstore.Message {
	return sessionstore.Message{
		Seq:         seq,
		Role:        "tool",
		ToolCallIDs: []string{callID},
		Parts: []sessionstore.MessagePart{
			{Type: sessionstore.PartTypeToolResult, ToolResult: &sessionstore.ToolResultSpec{
				ToolCallID: callID,
				Parts:      []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "result"}},
			}},
		},
	}
}

func TestApplyPrepared_RejectsOrphanCutoff(t *testing.T) {
	msgs := []sessionstore.Message{
		msgText(1, "user", "hello"),
		msgToolCall(2, "A"),
		msgToolResult(3, "A"),
		msgText(4, "user", "next"),
	}
	if _, _, ok := ApplyPrepared(msgs, 2, "S"); ok {
		t.Fatal("ApplyPrepared must REJECT a cutoff that strands an orphan tool result")
	}
}

func TestApplyPrepared_SafeCutoffApplies(t *testing.T) {
	msgs := []sessionstore.Message{
		msgText(1, "user", "hello"),
		msgToolCall(2, "A"),
		msgToolResult(3, "A"),
		msgText(4, "user", "next"),
	}
	view, dropped, ok := ApplyPrepared(msgs, 1, "SUMMARY")
	if !ok {
		t.Fatal("safe cutoff was rejected")
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
	if len(view) == 0 || view[0].Role != "system" || view[0].Content != "SUMMARY" {
		t.Errorf("summary not prepended as a system message: %+v", view)
	}
	if len(view) != 4 {
		t.Errorf("view len = %d, want 4 (summary + 3 kept)", len(view))
	}
}

func TestApplyPrepared_ZeroCutoff(t *testing.T) {
	msgs := []sessionstore.Message{msgText(1, "user", "a"), msgText(2, "user", "b")}
	if _, _, ok := ApplyPrepared(msgs, 0, "S"); ok {
		t.Fatal("cutoff 0 must be a no-op (ok=false)")
	}
}

func TestApplyPrepared_KeepsUnsequenced(t *testing.T) {
	msgs := []sessionstore.Message{
		msgText(1, "user", "old"),
		msgText(0, "system", "just-injected"),
		msgText(2, "user", "recent"),
	}
	view, dropped, ok := ApplyPrepared(msgs, 1, "S")
	if !ok || dropped != 1 {
		t.Fatalf("ok=%v dropped=%d, want ok=true dropped=1", ok, dropped)
	}
	var sawInjected bool
	for _, m := range view {
		if m.Seq == 0 && m.Content == "" {
			for _, p := range m.Parts {
				if p.Text == "just-injected" {
					sawInjected = true
				}
			}
		}
	}
	if !sawInjected {
		t.Error("Seq==0 message was dropped — unsequenced messages must always survive")
	}
}
