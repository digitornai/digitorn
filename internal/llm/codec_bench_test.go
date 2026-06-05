package llm

import (
	"encoding/json"
	"testing"

	"github.com/bytedance/sonic"
)

// Realistic ChatRequest payload: 8 messages with mix of text + tool calls.
// This is the shape the gRPC codec marshals on every request.
func benchPayload() *ChatRequest {
	return &ChatRequest{
		Provider: "anthropic", Model: "claude-sonnet-4.5", BYOK: true,
		APIKey: "sk-ant-test-1234567890abcdef",
		Messages: []ChatMessage{
			{Role: "system", Content: "You are a careful coding assistant. Be terse."},
			{Role: "user", Content: "Refactor this function for clarity."},
			{Role: "assistant", Content: "Sure. Let me read it first."},
			{Role: "assistant", ToolCalls: []ChatToolCall{{
				ID: "call_001", Type: "function", Name: "read_file",
				Arguments: map[string]any{"path": "/src/main.go", "limit": 200},
			}}},
			{Role: "tool", ToolCallID: "call_001", Content: "package main\n\nfunc main(){...}"},
			{Role: "assistant", Content: "Here is the refactored version: ..."},
			{Role: "user", Content: "Now write tests for it."},
			{Role: "assistant", Content: "Drafting tests."},
		},
	}
}

func BenchmarkCodec_Sonic_Marshal(b *testing.B) {
	b.ReportAllocs()
	p := benchPayload()
	for i := 0; i < b.N; i++ {
		if _, err := sonic.Marshal(p); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCodec_StdJSON_Marshal(b *testing.B) {
	b.ReportAllocs()
	p := benchPayload()
	for i := 0; i < b.N; i++ {
		if _, err := json.Marshal(p); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCodec_Sonic_Unmarshal(b *testing.B) {
	b.ReportAllocs()
	data, _ := sonic.Marshal(benchPayload())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var out ChatRequest
		if err := sonic.Unmarshal(data, &out); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCodec_StdJSON_Unmarshal(b *testing.B) {
	b.ReportAllocs()
	data, _ := json.Marshal(benchPayload())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var out ChatRequest
		if err := json.Unmarshal(data, &out); err != nil {
			b.Fatal(err)
		}
	}
}
