package server

import (
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// TestRenderTranscript_DescribesToolCalls: the summariser input must describe the
// tool calls (name + args) and name the results via the call id — the work done,
// not just chatter.
func TestRenderTranscript_DescribesToolCalls(t *testing.T) {
	msgs := []sessionstore.Message{
		{Role: "user", Content: "read the config"},
		{Role: "assistant", Content: "ok", Parts: []sessionstore.MessagePart{
			{Type: sessionstore.PartTypeText, Text: "ok"},
			{Type: sessionstore.PartTypeToolCall, ToolCall: &sessionstore.ToolCallSpec{ID: "c1", Name: "read_file", Args: map[string]any{"path": "/etc/config"}}},
		}},
		{Role: "tool", ToolCallIDs: []string{"c1"}, Content: "key=value", Parts: []sessionstore.MessagePart{
			{Type: sessionstore.PartTypeToolResult, ToolResult: &sessionstore.ToolResultSpec{ToolCallID: "c1", Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "key=value"}}}},
		}},
	}
	out := renderTranscript(msgs)
	if !strings.Contains(out, "read_file(") || !strings.Contains(out, "path=/etc/config") {
		t.Errorf("transcript missing tool-call description:\n%s", out)
	}
	if !strings.Contains(out, "tool read_file result:") || !strings.Contains(out, "key=value") {
		t.Errorf("transcript missing named tool result:\n%s", out)
	}
}

// TestRenderTranscript_SurfacesError: a failed tool result must keep its error in
// the transcript so the summary records what went wrong.
func TestRenderTranscript_SurfacesError(t *testing.T) {
	msgs := []sessionstore.Message{
		{Role: "assistant", Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeToolCall, ToolCall: &sessionstore.ToolCallSpec{ID: "c1", Name: "run"}}}},
		{Role: "tool", Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeToolResult, ToolResult: &sessionstore.ToolResultSpec{ToolCallID: "c1", Error: "permission denied"}}}},
	}
	out := renderTranscript(msgs)
	if !strings.Contains(out, "[ERROR]") || !strings.Contains(out, "permission denied") {
		t.Errorf("transcript missing error:\n%s", out)
	}
}

// TestRenderTranscript_ClipsBigOutput: a huge tool output is clipped so the
// summariser input stays bounded.
func TestRenderTranscript_ClipsBigOutput(t *testing.T) {
	big := strings.Repeat("z", 10000)
	msgs := []sessionstore.Message{
		{Role: "tool", Content: big, Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeToolResult, ToolResult: &sessionstore.ToolResultSpec{ToolCallID: "c1", Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: big}}}}}},
	}
	out := renderTranscript(msgs)
	if len(out) > 3000 {
		t.Errorf("big tool output not clipped: transcript len %d", len(out))
	}
	if !strings.Contains(out, "…") {
		t.Error("clip marker missing")
	}
}

// TestRenderTranscript_PlainText: ordinary messages still render as "role: text".
func TestRenderTranscript_PlainText(t *testing.T) {
	msgs := []sessionstore.Message{
		{Role: "user", Content: "hello there"},
		{Role: "assistant", Content: "hi back"},
	}
	out := renderTranscript(msgs)
	if !strings.Contains(out, "user: hello there") || !strings.Contains(out, "assistant: hi back") {
		t.Errorf("plain text not rendered:\n%s", out)
	}
}
