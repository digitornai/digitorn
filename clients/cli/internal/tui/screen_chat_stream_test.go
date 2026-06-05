package tui

import (
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn-cli/internal/client"
	"github.com/mbathepaul/digitorn-cli/internal/theme"
)

// Replays a realistic streamed turn through handleEnvelope and asserts
// the final assistant message remains visible after the stream clears.
func TestHandleEnvelope_StreamedTurnShowsFinal(t *testing.T) {
	s := &ChatScreen{theme: theme.Default(), messages: NewMessages(theme.Default()), sessionID: "sess1"}
	s.messages.SetSize(80, 20)

	feed := func(typ string, seq uint64, payload map[string]any) {
		s.handleEnvelope(client.Envelope{Type: typ, SessionID: "sess1", Seq: seq, Payload: payload})
	}
	textParts := func(txt string) map[string]any {
		return map[string]any{"parts": []any{map[string]any{"type": "text", "text": txt}}}
	}

	feed("turn_started", 1, nil)
	feed("user_message", 2, map[string]any{"content": "hi"})
	feed("assistant_delta", 3, textParts("Hel"))
	feed("assistant_delta", 4, textParts("lo!"))
	feed("assistant_message", 5, map[string]any{"content": "Hello!"})
	feed("turn_ended", 6, map[string]any{"status": "done"})

	v := s.messages.View()
	if !strings.Contains(stripANSI(v), "Hello!") {
		t.Fatalf("final assistant message not visible after streamed turn:\n%q", v)
	}
	if strings.Contains(v, "▌") {
		t.Fatalf("streaming cursor still present after turn_ended:\n%q", v)
	}
}

// A tool_call chip must update IN PLACE when its tool_result lands
// (matched by call_id) — running → completed + duration, no duplicate.
func TestHandleEnvelope_ToolChipUpdatesByCallID(t *testing.T) {
	s := &ChatScreen{theme: theme.Default(), messages: NewMessages(theme.Default()), sessionID: "sess1"}
	s.messages.SetSize(80, 20)
	feed := func(typ string, seq uint64, payload map[string]any) {
		s.handleEnvelope(client.Envelope{Type: typ, SessionID: "sess1", Seq: seq, Payload: payload})
	}

	// Chips display the human verb ("Read"), not the FQN.
	feed("tool_call", 3, map[string]any{"call_id": "c1", "name": "filesystem.read", "status": "pending"})
	if v := s.messages.View(); !strings.Contains(v, "Read") || !strings.Contains(v, "running") {
		t.Fatalf("running tool chip not shown:\n%q", v)
	}

	feed("tool_result", 4, map[string]any{"call_id": "c1", "name": "filesystem.read", "status": "completed", "duration_ms": float64(12)})
	v := s.messages.View()
	if strings.Contains(v, "running") {
		t.Fatalf("chip still 'running' after result:\n%q", v)
	}
	if !strings.Contains(v, "12ms") {
		t.Fatalf("duration not shown after result:\n%q", v)
	}
	if n := strings.Count(v, "Read"); n != 1 {
		t.Fatalf("expected one chip (updated in place), got %d:\n%q", n, v)
	}
}

// A tool chip shows the arg hint (from tool_call args) and an output
// preview (from tool_result), and resolves the real tool through the
// execute_tool wrapper.
func TestHandleEnvelope_ToolChipArgAndOutput(t *testing.T) {
	s := &ChatScreen{theme: theme.Default(), messages: NewMessages(theme.Default()), sessionID: "sess1"}
	s.messages.SetSize(80, 20)
	feed := func(typ string, seq uint64, payload map[string]any) {
		s.handleEnvelope(client.Envelope{Type: typ, SessionID: "sess1", Seq: seq, Payload: payload})
	}

	// Reached via execute_tool : name resolves to filesystem.read, arg to path.
	feed("tool_call", 3, map[string]any{
		"call_id":   "c1",
		"name":      "context_builder__execute_tool",
		"arguments": map[string]any{"name": "filesystem.read", "params": map[string]any{"path": "seed.txt"}},
	})
	feed("tool_result", 4, map[string]any{
		"call_id":     "c1",
		"name":        "context_builder__execute_tool",
		"status":      "completed",
		"duration_ms": float64(3),
		"parts":       []any{map[string]any{"type": "text", "text": "the magic number is 4242"}},
	})

	// Collapsed by default : the summary (action + arg) shows, the output
	// does NOT, and a caret + line hint advertise that it's expandable.
	v := stripANSI(s.messages.View())
	for _, want := range []string{"Read", "seed.txt", "▸"} {
		if !strings.Contains(v, want) {
			t.Fatalf("collapsed chip missing %q:\n%q", want, v)
		}
	}
	if strings.Contains(v, "the magic number is 4242") {
		t.Fatalf("output should be hidden until expanded:\n%q", v)
	}
	// Clicking the chip (row 0 of the messages area) expands it.
	if !s.messages.ToggleAt(0) {
		t.Fatal("ToggleAt(0) did not hit the tool chip")
	}
	v = stripANSI(s.messages.View())
	if !strings.Contains(v, "the magic number is 4242") {
		t.Fatalf("output not revealed after expand:\n%q", v)
	}
	if !strings.Contains(v, "▾") {
		t.Fatalf("expanded caret missing:\n%q", v)
	}
	// Clicking again collapses it back.
	s.messages.ToggleAt(0)
	if strings.Contains(stripANSI(s.messages.View()), "the magic number is 4242") {
		t.Fatal("output should hide again after second click")
	}
}
