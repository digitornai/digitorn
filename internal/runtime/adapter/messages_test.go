package adapter_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime/adapter"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// captureReporter records every Warn call so tests can assert what was
// dropped / skipped during conversion.
type captureReporter struct {
	calls []string
}

func (r *captureReporter) Warn(msg string, kv ...any) {
	r.calls = append(r.calls, msg)
}

func makeBlobLoader(blobs map[string][]byte) adapter.BlobLoader {
	return func(_ context.Context, hash string) ([]byte, error) {
		return blobs[hash], nil
	}
}

// ---- legacy text path (must keep working bit-for-bit) ----------------

func TestMessagesToLLM_Empty(t *testing.T) {
	if got := adapter.MessagesToLLM(context.Background(), nil, adapter.Options{}); got != nil {
		t.Errorf("nil input → %v, want nil", got)
	}
	if got := adapter.MessagesToLLM(context.Background(), []sessionstore.Message{}, adapter.Options{}); len(got) != 0 {
		t.Errorf("empty input → %v, want empty", got)
	}
}

func TestMessagesToLLM_PreservesOrderAndContent(t *testing.T) {
	in := []sessionstore.Message{
		{Seq: 1, Role: "system", Content: "you are helpful"},
		{Seq: 2, Role: "user", Content: "hi"},
		{Seq: 3, Role: "assistant", Content: "hello there"},
		{Seq: 4, Role: "user", Content: "what's 2+2?"},
		{Seq: 5, Role: "assistant", Content: "4"},
	}
	out := adapter.MessagesToLLM(context.Background(), in, adapter.Options{})
	if len(out) != 5 {
		t.Fatalf("len = %d, want 5", len(out))
	}
	for i, want := range []struct{ role, content string }{
		{"system", "you are helpful"},
		{"user", "hi"},
		{"assistant", "hello there"},
		{"user", "what's 2+2?"},
		{"assistant", "4"},
	} {
		if out[i].Role != want.role || out[i].Content != want.content {
			t.Errorf("out[%d] = %+v, want role=%q content=%q",
				i, out[i], want.role, want.content)
		}
	}
}

func TestMessagesToLLM_DropsUnknownRoles(t *testing.T) {
	rep := &captureReporter{}
	in := []sessionstore.Message{
		{Role: "user", Content: "hi"},
		{Role: "moderator", Content: "agent should not see this"},
		{Role: "assistant", Content: "hello"},
		{Role: "", Content: "no role at all"},
	}
	out := adapter.MessagesToLLM(context.Background(), in, adapter.Options{Report: rep})
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2 ; got = %+v", len(out), out)
	}
	if out[0].Role != "user" || out[1].Role != "assistant" {
		t.Errorf("unexpected order : %+v", out)
	}
	if len(rep.calls) == 0 {
		t.Errorf("expected Warn for unknown roles, got none")
	}
}

func TestMessagesToLLM_AcceptsToolRole_LegacyContent(t *testing.T) {
	// Old-style "tool" message with only Content : we have no
	// ToolCallID to attach (Parts is empty) so it falls into the
	// generic skip path. Verify the conversation still flows for the
	// surrounding user/assistant turns.
	in := []sessionstore.Message{
		{Role: "user", Content: "list files"},
		{Role: "assistant", Content: "I'll do that."},
		{Role: "tool", Content: "[a.txt, b.txt]"}, // legacy : no Parts, no ToolCallID
		{Role: "assistant", Content: "Found a.txt and b.txt."},
	}
	out := adapter.MessagesToLLM(context.Background(), in, adapter.Options{})
	// Without a ToolResult part the tool message has nothing to render.
	// Surrounding user/assistant survive.
	if len(out) < 3 {
		t.Fatalf("expected at least user+assistant+assistant, got %d : %+v", len(out), out)
	}
}

// ---- multipart : image inlining --------------------------------------

func TestMessagesToLLM_UserImage_LoadsBlobInline(t *testing.T) {
	blobs := map[string][]byte{"img-hash": []byte("\x89PNG fake bytes")}
	msgs := []sessionstore.Message{
		{
			Role: "user",
			Parts: []sessionstore.MessagePart{
				{Type: sessionstore.PartTypeText, Text: "what's in this image?"},
				{Type: sessionstore.PartTypeImage, Blob: &sessionstore.BlobRef{
					Hash: "img-hash", Mime: "image/png", Size: 16,
				}},
			},
		},
	}
	got := adapter.MessagesToLLM(context.Background(), msgs, adapter.Options{
		LoadBlob: makeBlobLoader(blobs),
	})
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	if len(got[0].Parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(got[0].Parts))
	}
	if got[0].Parts[1].Type != llm.ContentTypeImage {
		t.Fatalf("expected image part, got %q", got[0].Parts[1].Type)
	}
	if string(got[0].Parts[1].Data) != "\x89PNG fake bytes" {
		t.Fatalf("blob bytes not inlined : %v", got[0].Parts[1].Data)
	}
	if got[0].Parts[1].Mime != "image/png" {
		t.Fatalf("mime lost : %q", got[0].Parts[1].Mime)
	}
}

func TestMessagesToLLM_NoBlobLoader_ImageSkippedWithWarn(t *testing.T) {
	rep := &captureReporter{}
	msgs := []sessionstore.Message{
		{
			Role: "user",
			Parts: []sessionstore.MessagePart{
				{Type: sessionstore.PartTypeText, Text: "before"},
				{Type: sessionstore.PartTypeImage, Blob: &sessionstore.BlobRef{Hash: "h", Mime: "image/jpg"}},
				{Type: sessionstore.PartTypeText, Text: "after"},
			},
		},
	}
	got := adapter.MessagesToLLM(context.Background(), msgs, adapter.Options{Report: rep})
	if len(got) != 1 {
		t.Fatalf("expected 1 msg, got %d", len(got))
	}
	if len(got[0].Parts) != 2 {
		t.Fatalf("expected 2 surviving text parts, got %d", len(got[0].Parts))
	}
	if len(rep.calls) == 0 {
		t.Fatal("expected a Warn for the skipped blob part")
	}
}

// ---- tool calls + results --------------------------------------------

func TestMessagesToLLM_AssistantToolCall(t *testing.T) {
	msgs := []sessionstore.Message{
		{
			Role: "assistant",
			Parts: []sessionstore.MessagePart{
				{Type: sessionstore.PartTypeText, Text: "Let me search..."},
				{Type: sessionstore.PartTypeToolCall, ToolCall: &sessionstore.ToolCallSpec{
					ID: "call-1", Name: "web_search",
					Args: map[string]any{"q": "weather paris"},
				}},
			},
		},
	}
	got := adapter.MessagesToLLM(context.Background(), msgs, adapter.Options{})
	if len(got) != 1 {
		t.Fatalf("expected 1 msg, got %d", len(got))
	}
	if got[0].Content != "Let me search..." {
		t.Fatalf("text content lost : %q", got[0].Content)
	}
	if len(got[0].ToolCalls) != 1 {
		t.Fatalf("tool call not extracted : %+v", got[0].ToolCalls)
	}
	if got[0].ToolCalls[0].ID != "call-1" || got[0].ToolCalls[0].Name != "web_search" {
		t.Fatalf("tool call fields lost : %+v", got[0].ToolCalls[0])
	}
	if v, ok := got[0].ToolCalls[0].Arguments["q"].(string); !ok || v != "weather paris" {
		t.Fatalf("tool call args lost : %+v", got[0].ToolCalls[0].Arguments)
	}
}

func TestMessagesToLLM_RepairsDanglingToolCallOnResume(t *testing.T) {
	// A turn aborted (or daemon crashed) while a tool ran : the assistant's
	// tool_call is durable but its result is missing. On the NEXT turn (a new
	// user message follows) the adapter must synthesize a terminal result so the
	// provider doesn't reject the whole request for an unanswered tool_call_id.
	msgs := []sessionstore.Message{
		{Role: "user", Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "do X"}}},
		{Role: "assistant", Parts: []sessionstore.MessagePart{
			{Type: sessionstore.PartTypeToolCall, ToolCall: &sessionstore.ToolCallSpec{
				ID: "call-dangling", Name: "filesystem.read",
			}},
		}},
		// no tool result here — interrupted mid-dispatch
		{Role: "user", Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "actually, never mind — do Y"}}},
	}
	got := adapter.MessagesToLLM(context.Background(), msgs, adapter.Options{})

	// Expect: user, assistant(tool_call), SYNTHETIC tool result, user.
	if len(got) != 4 {
		t.Fatalf("expected 4 messages (synthetic result inserted), got %d : %+v", len(got), got)
	}
	if got[1].Role != "assistant" || len(got[1].ToolCalls) != 1 {
		t.Fatalf("assistant tool_call lost : %+v", got[1])
	}
	if got[2].Role != "tool" || got[2].ToolCallID != "call-dangling" {
		t.Fatalf("synthetic tool result not paired to the dangling call : %+v", got[2])
	}
	if !strings.Contains(got[2].Content, "interrupted") {
		t.Errorf("synthetic result should explain the interruption : %q", got[2].Content)
	}
	if got[3].Role != "user" {
		t.Fatalf("trailing user message lost : %+v", got[3])
	}
}

func TestMessagesToLLM_PullsResultForwardOverInterleavedMessage(t *testing.T) {
	// The approval flow interleaves a user message (the user's "approve" reply)
	// between an assistant's gated tool_call and the tool's result :
	//   assistant(tool_call) → user("approve") → tool(result) → system(note)
	// Providers require the tool result to IMMEDIATELY follow the tool_call.
	// DeepSeek rejects the gap ("tool must be a response to a preceding message
	// with tool_calls"). The adapter must pull the result forward so the wire
	// reads assistant → tool → user → system.
	msgs := []sessionstore.Message{
		{Role: "user", Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "run it"}}},
		{Role: "assistant", Parts: []sessionstore.MessagePart{
			{Type: sessionstore.PartTypeToolCall, ToolCall: &sessionstore.ToolCallSpec{ID: "call-gated", Name: "bash.run"}},
		}},
		{Role: "user", Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "approve"}}},
		{Role: "tool", Parts: []sessionstore.MessagePart{
			{Type: sessionstore.PartTypeToolResult, ToolResult: &sessionstore.ToolResultSpec{
				ToolCallID: "call-gated",
				Parts:      []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "done"}},
			}},
		}},
		{Role: "system", Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "note"}}},
	}
	got := adapter.MessagesToLLM(context.Background(), msgs, adapter.Options{})

	if len(got) != 5 {
		t.Fatalf("expected 5 messages (no synthetic, just reorder), got %d : %+v", len(got), got)
	}
	// Invariant : every tool message is immediately preceded by an assistant
	// carrying its tool_call_id.
	for i, g := range got {
		if g.Role != "tool" {
			continue
		}
		if i == 0 || got[i-1].Role != "assistant" {
			t.Fatalf("tool message at %d not preceded by assistant : %+v", i, got)
		}
		ok := false
		for _, tc := range got[i-1].ToolCalls {
			if tc.ID == g.ToolCallID {
				ok = true
			}
		}
		if !ok {
			t.Fatalf("tool %q not answered by preceding assistant tool_calls : %+v", g.ToolCallID, got)
		}
	}
	if got[1].Role != "assistant" || got[2].Role != "tool" || got[2].ToolCallID != "call-gated" {
		t.Fatalf("result not pulled adjacent to its tool_call : %+v", got)
	}
	// The interleaved user + system fall in after the tool block, order kept.
	if got[3].Role != "user" || got[3].Content != "approve" || got[4].Role != "system" {
		t.Fatalf("interleaved messages not preserved after tool block : %+v", got)
	}
	// Exactly one tool message — no synthetic duplicate.
	tools := 0
	for _, g := range got {
		if g.Role == "tool" {
			tools++
		}
	}
	if tools != 1 {
		t.Fatalf("expected exactly 1 tool message, got %d", tools)
	}
}

func TestMessagesToLLM_PairedToolCallUntouched(t *testing.T) {
	// A properly answered tool_call must NOT get a synthetic duplicate.
	msgs := []sessionstore.Message{
		{Role: "assistant", Parts: []sessionstore.MessagePart{
			{Type: sessionstore.PartTypeToolCall, ToolCall: &sessionstore.ToolCallSpec{ID: "call-1", Name: "x"}},
		}},
		{Role: "tool", Parts: []sessionstore.MessagePart{
			{Type: sessionstore.PartTypeToolResult, ToolResult: &sessionstore.ToolResultSpec{
				ToolCallID: "call-1",
				Parts:      []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "ok"}},
			}},
		}},
		{Role: "user", Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "next"}}},
	}
	got := adapter.MessagesToLLM(context.Background(), msgs, adapter.Options{})
	if len(got) != 3 {
		t.Fatalf("paired conversation must be unchanged, got %d msgs : %+v", len(got), got)
	}
	toolMsgs := 0
	for _, g := range got {
		if g.Role == "tool" {
			toolMsgs++
		}
	}
	if toolMsgs != 1 {
		t.Errorf("expected exactly 1 tool message, got %d (synthetic duplicate?)", toolMsgs)
	}
}

func TestMessagesToLLM_ToolResult_TextOnly_CollapsesToContent(t *testing.T) {
	msgs := []sessionstore.Message{
		{
			Role: "tool",
			Parts: []sessionstore.MessagePart{
				{Type: sessionstore.PartTypeToolResult, ToolResult: &sessionstore.ToolResultSpec{
					ToolCallID: "call-1",
					Parts: []sessionstore.MessagePart{
						{Type: sessionstore.PartTypeText, Text: "search results JSON ..."},
					},
				}},
			},
		},
	}
	got := adapter.MessagesToLLM(context.Background(), msgs, adapter.Options{})
	if len(got) != 1 || got[0].Role != "tool" {
		t.Fatalf("expected 1 tool msg, got %+v", got)
	}
	if got[0].ToolCallID != "call-1" {
		t.Fatalf("ToolCallID lost : %q", got[0].ToolCallID)
	}
	if got[0].Content != "search results JSON ..." {
		t.Fatalf("simple text result collapsed wrongly : %+v", got[0])
	}
}

func TestMessagesToLLM_ToolResult_TextAndImage_KeepsBothAsParts(t *testing.T) {
	blobs := map[string][]byte{"sshot": []byte("png-data")}
	msgs := []sessionstore.Message{
		{
			Role: "tool",
			Parts: []sessionstore.MessagePart{
				{Type: sessionstore.PartTypeToolResult, ToolResult: &sessionstore.ToolResultSpec{
					ToolCallID: "sshot-1",
					Parts: []sessionstore.MessagePart{
						{Type: sessionstore.PartTypeText, Text: "Screenshot of the homepage."},
						{Type: sessionstore.PartTypeImage, Blob: &sessionstore.BlobRef{
							Hash: "sshot", Mime: "image/png", Size: 8,
						}},
					},
				}},
			},
		},
	}
	got := adapter.MessagesToLLM(context.Background(), msgs, adapter.Options{
		LoadBlob: makeBlobLoader(blobs),
	})
	if len(got) != 1 {
		t.Fatalf("expected 1 msg, got %d", len(got))
	}
	if got[0].Role != "tool" || got[0].ToolCallID != "sshot-1" {
		t.Fatalf("tool message fields lost : %+v", got[0])
	}
	if len(got[0].Parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(got[0].Parts))
	}
	if got[0].Parts[0].Type != llm.ContentTypeText {
		t.Fatalf("first part should be text : %+v", got[0].Parts[0])
	}
	if got[0].Parts[1].Type != llm.ContentTypeImage || string(got[0].Parts[1].Data) != "png-data" {
		t.Fatalf("image part lost or empty : %+v", got[0].Parts[1])
	}
}

func TestMessagesToLLM_MultipleResults_OneMessageEach(t *testing.T) {
	msgs := []sessionstore.Message{
		{
			Role: "tool",
			Parts: []sessionstore.MessagePart{
				{Type: sessionstore.PartTypeToolResult, ToolResult: &sessionstore.ToolResultSpec{
					ToolCallID: "call-a",
					Parts:      []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "A"}},
				}},
				{Type: sessionstore.PartTypeToolResult, ToolResult: &sessionstore.ToolResultSpec{
					ToolCallID: "call-b",
					Parts:      []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "B"}},
				}},
			},
		},
	}
	got := adapter.MessagesToLLM(context.Background(), msgs, adapter.Options{})
	if len(got) != 2 {
		t.Fatalf("expected 2 tool messages, got %d : %+v", len(got), got)
	}
	if got[0].ToolCallID != "call-a" || got[1].ToolCallID != "call-b" {
		t.Fatalf("tool call IDs mis-paired : %+v", got)
	}
}

func TestMessagesToLLM_ToolError_AppendedAsText(t *testing.T) {
	msgs := []sessionstore.Message{
		{
			Role: "tool",
			Parts: []sessionstore.MessagePart{
				{Type: sessionstore.PartTypeToolResult, ToolResult: &sessionstore.ToolResultSpec{
					ToolCallID: "fail-1",
					Error:      "rate limit exceeded",
				}},
			},
		},
	}
	got := adapter.MessagesToLLM(context.Background(), msgs, adapter.Options{})
	if len(got) != 1 {
		t.Fatalf("expected 1 msg, got %d", len(got))
	}
	if !strings.Contains(got[0].Content, "rate limit exceeded") {
		t.Fatalf("tool error not surfaced in content : %q", got[0].Content)
	}
}

func TestMessagesToLLM_LegacyAttachments_StillVisible(t *testing.T) {
	blobs := map[string][]byte{"old-img": []byte("legacy bytes")}
	msgs := []sessionstore.Message{
		{
			Role:    "user",
			Content: "look at this",
			Attachments: []sessionstore.BlobRef{
				{Hash: "old-img", Mime: "image/png", Size: 12},
			},
		},
	}
	got := adapter.MessagesToLLM(context.Background(), msgs, adapter.Options{
		LoadBlob: makeBlobLoader(blobs),
	})
	if len(got) != 1 {
		t.Fatalf("expected 1 msg, got %d", len(got))
	}
	if len(got[0].Parts) != 2 {
		t.Fatalf("expected 2 parts (text + image), got %d : %+v", len(got[0].Parts), got[0].Parts)
	}
	if string(got[0].Parts[1].Data) != "legacy bytes" {
		t.Fatalf("legacy attachment bytes not loaded : %v", got[0].Parts[1].Data)
	}
}

// ---- PrependSystemPrompt ---------------------------------------------

func TestPrependSystemPrompt_EmptyPromptIsNoop(t *testing.T) {
	in := []llm.ChatMessage{{Role: "user", Content: "hi"}}
	out := adapter.PrependSystemPrompt(in, "")
	if len(out) != 1 || out[0].Role != "user" {
		t.Errorf("empty prompt mutated input : %+v", out)
	}
}

func TestPrependSystemPrompt_InsertsAtPositionZero(t *testing.T) {
	in := []llm.ChatMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	out := adapter.PrependSystemPrompt(in, "you are helpful")
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	if out[0].Role != "system" || out[0].Content != "you are helpful" {
		t.Errorf("system message not inserted at 0 : %+v", out[0])
	}
}

func TestPrependSystemPrompt_ReplacesExistingSystem(t *testing.T) {
	in := []llm.ChatMessage{
		{Role: "system", Content: "old prompt"},
		{Role: "user", Content: "hi"},
	}
	out := adapter.PrependSystemPrompt(in, "new prompt")
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2 (no stacking)", len(out))
	}
	if out[0].Content != "new prompt" {
		t.Errorf("system not replaced : %+v", out[0])
	}
}

func TestPrependSystemPrompt_Multipart_Safe(t *testing.T) {
	msgs := []llm.ChatMessage{
		{Role: "user", Parts: []llm.ContentPart{
			{Type: llm.ContentTypeText, Text: "hi"},
			{Type: llm.ContentTypeImage, Mime: "image/png", Data: []byte("x")},
		}},
	}
	got := adapter.PrependSystemPrompt(msgs, "you are a vision assistant")
	if len(got) != 2 || got[0].Role != "system" {
		t.Fatalf("system prompt not prepended : %+v", got)
	}
	if got[0].Content != "you are a vision assistant" {
		t.Fatalf("prompt content wrong : %q", got[0].Content)
	}
	if len(got[1].Parts) != 2 {
		t.Fatalf("downstream multipart corrupted : %+v", got[1])
	}
}
