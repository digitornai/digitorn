package bifrost

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/providers/openai"
	"github.com/mbathepaul/digitorn/internal/llm"
)

// An assistant message carrying ReasoningContent must reach the provider as
// reasoning_content. DeepSeek thinking mode rejects a turn whose prior
// reasoning_content was dropped — this proves the outbound round-trip all the
// way to the OpenAI-compatible wire form (what the DeepSeek gateway sees).
func TestBuildChatRequest_AssistantReasoningRoundTrip(t *testing.T) {
	s := &Service{}
	req := &llm.ChatRequest{
		Provider: "openai",
		Model:    "deepseek-v4-flash",
		Messages: []llm.ChatMessage{{
			Role:             "assistant",
			ReasoningContent: "I should locate the files first.",
			ToolCalls:        []llm.ChatToolCall{{ID: "c1", Name: "filesystem.glob"}},
		}},
	}

	out := s.buildChatRequest(req)
	if len(out.Input) != 1 || out.Input[0].ChatAssistantMessage == nil {
		t.Fatalf("assistant message not built: %+v", out.Input)
	}
	am := out.Input[0].ChatAssistantMessage
	if am.Reasoning == nil || *am.Reasoning != "I should locate the files first." {
		t.Fatalf("reasoning not set on bifrost assistant msg: %+v", am.Reasoning)
	}

	// The OpenAI-compatible wire form (what the DeepSeek gateway receives)
	// must carry the field as `reasoning_content`, not `reasoning`.
	wire, err := json.Marshal(openai.ConvertBifrostMessagesToOpenAIMessages(out.Input))
	if err != nil {
		t.Fatalf("marshal wire: %v", err)
	}
	if !strings.Contains(string(wire), `"reasoning_content"`) {
		t.Fatalf("outbound wire form missing reasoning_content:\n%s", wire)
	}
}

// THE regression for the bug: an assistant message that carries tool_calls but
// whose reasoning trace is EMPTY must STILL reach the provider with a
// reasoning_content field present. DeepSeek thinking mode rejects the next request
// otherwise ("...must be passed back to the API"), which is what broke after a write.
func TestBuildChatRequest_ToolCallsAlwaysCarryReasoningField(t *testing.T) {
	s := &Service{}
	req := &llm.ChatRequest{
		Provider: "openai",
		Model:    "deepseek-v4-flash",
		Messages: []llm.ChatMessage{{
			Role:      "assistant",
			ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "filesystem.write"}},
			// ReasoningContent deliberately empty — none captured for this turn.
		}},
	}
	out := s.buildChatRequest(req)
	if len(out.Input) != 1 || out.Input[0].ChatAssistantMessage == nil {
		t.Fatalf("assistant message not built: %+v", out.Input)
	}
	if out.Input[0].ChatAssistantMessage.Reasoning == nil {
		t.Fatal("reasoning field must be present on a tool-call assistant message even when empty")
	}
	// Prove it survives all the way to the OpenAI-compatible wire (DeepSeek gateway).
	wire, err := json.Marshal(openai.ConvertBifrostMessagesToOpenAIMessages(out.Input))
	if err != nil {
		t.Fatalf("marshal wire: %v", err)
	}
	if !strings.Contains(string(wire), `"reasoning_content"`) {
		t.Fatalf("wire form for a tool-call message must carry reasoning_content:\n%s", wire)
	}
}

// An assistant message without reasoning leaves the field absent — no empty
// reasoning_content key that a strict provider could choke on.
func TestBuildChatRequest_NoReasoningNoField(t *testing.T) {
	s := &Service{}
	req := &llm.ChatRequest{
		Provider: "openai",
		Model:    "deepseek-v4-flash",
		Messages: []llm.ChatMessage{{Role: "assistant", Content: "done"}},
	}
	out := s.buildChatRequest(req)
	if len(out.Input) == 1 && out.Input[0].ChatAssistantMessage != nil &&
		out.Input[0].ChatAssistantMessage.Reasoning != nil {
		t.Fatalf("reasoning should be nil when none was set")
	}
}
