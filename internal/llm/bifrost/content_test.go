package bifrost

import (
	"testing"

	"github.com/digitornai/digitorn/internal/llm"
)

// An assistant message that only carries tool_calls has empty text. DeepSeek's
// strict deserializer rejects a message with the `content` field absent, so
// buildChatRequest must always populate it (empty string is fine everywhere).
func TestBuildChatRequest_ContentAlwaysPresent(t *testing.T) {
	s := &Service{}
	req := &llm.ChatRequest{
		Provider: "deepseek",
		Model:    "deepseek-v4-flash",
		Messages: []llm.ChatMessage{
			{Role: "user", Content: "hi"},
			{Role: "assistant", ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "filesystem.read"}}},
			{Role: "tool", ToolCallID: "c1", Content: "result"},
		},
	}
	out := s.buildChatRequest(req)
	if len(out.Input) != len(req.Messages) {
		t.Fatalf("got %d messages, want %d", len(out.Input), len(req.Messages))
	}
	for i := range out.Input {
		m := out.Input[i]
		if m.Content == nil || m.Content.ContentStr == nil {
			t.Fatalf("message[%d] role=%q has a nil content field — DeepSeek rejects missing `content`", i, m.Role)
		}
	}
	// The tool-call-only assistant message must carry an empty (not absent) content.
	if got := *out.Input[1].Content.ContentStr; got != "" {
		t.Fatalf("assistant tool_call message content = %q, want empty string", got)
	}
}
