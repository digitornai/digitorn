package tui

import (
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn-cli/internal/client"
	"github.com/mbathepaul/digitorn-cli/internal/theme"
)

// A streaming tool_call fires DURING the stream (low seq), but the assistant
// message stating the intent is persisted AFTER the stream (higher seq). The
// tool chip must still render AFTER the intent text — the chip floats to its
// latest (pending/result) seq. Regression lock for "tool jumped before the
// agent's message".
func TestHandleEnvelope_StreamingToolStaysAfterIntent(t *testing.T) {
	s := &ChatScreen{theme: theme.Default(), messages: NewMessages(theme.Default()), sessionID: "sess1"}
	s.messages.SetSize(80, 40)
	feed := func(typ string, seq uint64, payload map[string]any) {
		s.handleEnvelope(client.Envelope{Type: typ, SessionID: "sess1", Seq: seq, Payload: payload})
	}
	textParts := func(txt string) map[string]any {
		return map[string]any{"parts": []any{map[string]any{"type": "text", "text": txt}}}
	}

	feed("user_message", 2, map[string]any{"content": "clean up"})
	// Intent text streams first…
	feed("assistant_delta", 3, textParts("I will delete the old pages."))
	// …then the tool STREAMS (low seq, arrives before the durable message).
	feed("tool_call", 4, map[string]any{"call_id": "c1", "name": "filesystem.write", "status": "streaming", "live_tokens": float64(120)})
	feed("tool_call", 5, map[string]any{"call_id": "c1", "name": "filesystem.write", "status": "streaming", "live_tokens": float64(800)})
	// The durable intent message (higher seq than the streaming frames).
	feed("assistant_message", 6, map[string]any{"content": "I will delete the old pages."})
	// Then the call completes.
	feed("tool_call", 7, map[string]any{"call_id": "c1", "name": "filesystem.write", "status": "pending"})
	feed("tool_result", 8, map[string]any{"call_id": "c1", "name": "filesystem.write", "status": "completed", "duration_ms": float64(5)})

	snap := s.messagesSnapshot()
	iText, iTool := -1, -1
	for i := range snap {
		if snap[i].Role == "assistant" && strings.Contains(snap[i].Content, "delete the old pages") {
			iText = i
		}
		if snap[i].Role == "tool" && snap[i].CallID == "c1" {
			iTool = i
		}
	}
	if iText < 0 || iTool < 0 {
		t.Fatalf("missing intent(%d) or tool chip(%d) in %+v", iText, iTool, snap)
	}
	if iText > iTool {
		t.Fatalf("ORDER BROKEN: tool chip (idx %d) renders before the intent text (idx %d)", iTool, iText)
	}
	// Exactly one tool chip (no duplicate from streaming frames).
	n := 0
	for i := range snap {
		if snap[i].Role == "tool" && snap[i].CallID == "c1" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 tool chip, got %d", n)
	}
}

// The live tool overlay must SURVIVE the round's message commit — the tools it
// shows are about to execute, and a fast tool's pending→result window is too
// short to perceive as an inline chip alone, so the overlay is the user's only
// reliable "running" feedback. It is cleared per call_id on each result, and
// wholesale at the turn boundary so a streamed fragment that never became a real
// tool can't leak past the turn. Regression lock for BOTH "running tool looked
// skipped" and "◆ write streaming… stuck forever".
func TestHandleEnvelope_ToolOverlaySurvivesCommitClearsAtTurnEnd(t *testing.T) {
	s := &ChatScreen{theme: theme.Default(), messages: NewMessages(theme.Default()), sessionID: "s"}
	s.messages.SetSize(80, 40)
	feed := func(typ string, seq uint64, payload map[string]any) {
		s.handleEnvelope(client.Envelope{Type: typ, SessionID: "s", Seq: seq, Payload: payload})
	}

	// Two tools stream with call_ids that will NEVER get a matching pending.
	feed("tool_call", 3, map[string]any{"call_id": "stream-a", "name": "filesystem.write", "status": "streaming", "live_tokens": float64(154)})
	feed("tool_call", 4, map[string]any{"call_id": "stream-b", "name": "filesystem.write", "status": "streaming", "live_tokens": float64(74)})
	if len(s.streamingToolIDs) != 2 {
		t.Fatalf("expected 2 live streaming lines, got %d", len(s.streamingToolIDs))
	}

	// The round's message commits → the overlay must PERSIST (tools execute next).
	feed("assistant_message", 5, map[string]any{"content": "done"})
	if len(s.streamingToolIDs) != 2 {
		t.Fatalf("overlay must survive the message commit (tools still in flight), got %d", len(s.streamingToolIDs))
	}

	// A result clears just its own line.
	feed("tool_result", 6, map[string]any{"call_id": "stream-a", "name": "filesystem.write", "status": "completed", "duration_ms": float64(3)})
	if len(s.streamingToolIDs) != 1 {
		t.Fatalf("result must clear only its own overlay line, got %d", len(s.streamingToolIDs))
	}

	// The turn ends → the overlay is leak-proofed wholesale (stream-b never got a
	// pending/result, so only the turn boundary can drop it).
	feed("turn_ended", 7, map[string]any{"status": "completed"})
	if len(s.streamingToolIDs) != 0 {
		t.Fatalf("streaming overlay leaked past the turn: %v", s.streamingToolIDs)
	}
}

// The agent's intermediate text (a round that has BOTH a message and a tool
// call) must remain VISIBLE in the rendered view after the streamed round
// commits — the streaming overlay must not eat it. Regression lock for
// "I no longer see the agent's intermediate message".
func TestHandleEnvelope_IntermediateMessageVisibleInView(t *testing.T) {
	s := &ChatScreen{theme: theme.Default(), messages: NewMessages(theme.Default()), sessionID: "s"}
	s.messages.SetSize(80, 40)
	feed := func(typ string, seq uint64, payload map[string]any) {
		s.handleEnvelope(client.Envelope{Type: typ, SessionID: "s", Seq: seq, Payload: payload})
	}
	textParts := func(txt string) map[string]any {
		return map[string]any{"parts": []any{map[string]any{"type": "text", "text": txt}}}
	}

	feed("user_message", 1, map[string]any{"content": "build it"})
	feed("assistant_delta", 2, textParts("Je vais creer le projet."))
	feed("tool_call", 3, map[string]any{"call_id": "c1", "name": "filesystem.write", "status": "streaming", "live_tokens": float64(50)})
	feed("assistant_message", 4, map[string]any{"content": "Je vais creer le projet."})
	feed("tool_call", 5, map[string]any{"call_id": "c1", "name": "filesystem.write", "status": "pending"})
	feed("tool_result", 6, map[string]any{"call_id": "c1", "name": "filesystem.write", "status": "completed", "duration_ms": float64(4)})

	v := stripANSI(s.messages.View())
	if !strings.Contains(v, "Je vais creer le projet") {
		t.Fatalf("intermediate agent message NOT visible in view:\n%q", v)
	}
}
