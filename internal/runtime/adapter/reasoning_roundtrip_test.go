package adapter

import (
	"context"
	"testing"

	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// A persisted assistant message's reasoning trace must be replayed onto the
// llm.ChatMessage so the worker can pass it back to the provider. Reasoning
// models (DeepSeek thinking mode) reject a turn whose prior reasoning_content
// was dropped — this is the regression lock for that round-trip.
func TestMessagesToLLM_ReplaysReasoning(t *testing.T) {
	msgs := []sessionstore.Message{
		{
			Seq:       1,
			Role:      "assistant",
			Reasoning: "step 1: locate the files; step 2: call glob",
			Parts: []sessionstore.MessagePart{
				{Type: sessionstore.PartTypeToolCall, ToolCall: &sessionstore.ToolCallSpec{
					ID: "call_1", Name: "filesystem.glob",
				}},
			},
		},
	}

	out := MessagesToLLM(context.Background(), msgs, Options{})
	if len(out) != 1 {
		t.Fatalf("expected 1 message, got %d", len(out))
	}
	if out[0].ReasoningContent != "step 1: locate the files; step 2: call glob" {
		t.Fatalf("reasoning not replayed: got %q", out[0].ReasoningContent)
	}
	if len(out[0].ToolCalls) != 1 || out[0].ToolCalls[0].ID != "call_1" {
		t.Fatalf("tool_calls lost alongside reasoning: %+v", out[0].ToolCalls)
	}
}

// A message with no reasoning leaves the field empty — no spurious value.
func TestMessagesToLLM_NoReasoning(t *testing.T) {
	msgs := []sessionstore.Message{
		{Seq: 1, Role: "assistant", Content: "hello"},
	}
	out := MessagesToLLM(context.Background(), msgs, Options{})
	if len(out) != 1 || out[0].ReasoningContent != "" {
		t.Fatalf("unexpected reasoning: %+v", out)
	}
}
