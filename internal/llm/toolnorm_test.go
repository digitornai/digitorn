package llm

import "testing"

func TestNormalizeTextToolCalls_AnthropicXML(t *testing.T) {
	// The exact shape DeepSeek emitted in the field : Anthropic <function_calls>.
	r := &ChatResponse{
		Content: "I'll list the files.\n<function_calls>\n<invoke name=\"filesystem__ls\">\n<parameter name=\"path\" string=\"true\">.</parameter>\n</invoke>\n</function_calls>",
	}
	NormalizeTextToolCalls(r)
	if len(r.ToolCalls) != 1 {
		t.Fatalf("want 1 recovered tool call, got %d", len(r.ToolCalls))
	}
	tc := r.ToolCalls[0]
	if tc.Name != "filesystem__ls" {
		t.Fatalf("name=%q", tc.Name)
	}
	if tc.Type != "function" || tc.ID == "" {
		t.Fatalf("id/type not set: %+v", tc)
	}
	if tc.Arguments["path"] != "." {
		t.Fatalf("path arg=%v", tc.Arguments["path"])
	}
	if r.Content != "I'll list the files." {
		t.Fatalf("markup not stripped from content: %q", r.Content)
	}
}

func TestNormalizeTextToolCalls_NativeUntouched(t *testing.T) {
	// A response that already has native tool_calls must be left exactly as-is.
	native := []ChatToolCall{{ID: "x", Type: "function", Name: "fs.read", Arguments: map[string]any{"path": "a"}}}
	r := &ChatResponse{Content: "<function_calls><invoke name=\"y\"></invoke></function_calls>", ToolCalls: native}
	NormalizeTextToolCalls(r)
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].Name != "fs.read" {
		t.Fatalf("native tool calls were mutated: %+v", r.ToolCalls)
	}
}

func TestNormalizeTextToolCalls_PlainProseUntouched(t *testing.T) {
	r := &ChatResponse{Content: "Just a normal reply, no tools."}
	NormalizeTextToolCalls(r)
	if len(r.ToolCalls) != 0 {
		t.Fatalf("plain prose produced tool calls: %+v", r.ToolCalls)
	}
	if r.Content != "Just a normal reply, no tools." {
		t.Fatalf("plain prose mutated: %q", r.Content)
	}
}
