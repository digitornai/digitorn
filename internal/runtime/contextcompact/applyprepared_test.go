package contextcompact

import (
	"testing"

	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
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

// TestApplyPrepared_RejectsOrphanCutoff: a prepared cutoff that would keep a tool
// result whose tool_call is dropped must be rejected (ok=false) so the caller
// truncates instead — never an invalid (orphan tool_use_id) prompt.
func TestApplyPrepared_RejectsOrphanCutoff(t *testing.T) {
	msgs := []sessionstore.Message{
		msgText(1, "user", "hello"),
		msgToolCall(2, "A"),   // tool_call A
		msgToolResult(3, "A"), // tool_result A
		msgText(4, "user", "next"),
	}
	// cutoff=2 drops msg1+msg2 (incl. tool_call A) but keeps msg3 (tool_result A) → orphan.
	if _, _, ok := ApplyPrepared(msgs, 2, "S"); ok {
		t.Fatal("ApplyPrepared must REJECT a cutoff that strands an orphan tool result")
	}
}

// TestApplyPrepared_SafeCutoffApplies: an orphan-free cutoff applies, prepends the
// summary, and reports the dropped count.
func TestApplyPrepared_SafeCutoffApplies(t *testing.T) {
	msgs := []sessionstore.Message{
		msgText(1, "user", "hello"),
		msgToolCall(2, "A"),
		msgToolResult(3, "A"),
		msgText(4, "user", "next"),
	}
	// cutoff=1 drops only msg1; the A call+result pair stays together → safe.
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
	// kept = summary + msg2,3,4
	if len(view) != 4 {
		t.Errorf("view len = %d, want 4 (summary + 3 kept)", len(view))
	}
}

// TestApplyPrepared_ZeroCutoff: a zero cutoff (nothing prepared) is a no-op.
func TestApplyPrepared_ZeroCutoff(t *testing.T) {
	msgs := []sessionstore.Message{msgText(1, "user", "a"), msgText(2, "user", "b")}
	if _, _, ok := ApplyPrepared(msgs, 0, "S"); ok {
		t.Fatal("cutoff 0 must be a no-op (ok=false)")
	}
}

// TestApplyPrepared_KeepsUnsequenced: messages with Seq==0 (freshly injected) are
// always kept, never dropped by a cutoff.
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
