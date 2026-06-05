package tui

import (
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn-cli/internal/client"
	"github.com/mbathepaul/digitorn-cli/internal/theme"
)

func newSubAgentScreen() *ChatScreen {
	s := &ChatScreen{theme: theme.Default(), messages: NewMessages(theme.Default()), sessionID: "root"}
	s.messages.SetSize(80, 20)
	return s
}

// subAgentTrace flattens the sub-agent activity groups (kind + their live tool
// rows) so a test can assert a delegated agent's work was captured there — the
// rail tracks sub-agents as grouped activity, not as flat timeline rows.
func subAgentTrace(s *ChatScreen) string {
	var b strings.Builder
	for _, sa := range s.subActivity {
		b.WriteString(sa.kind + ":")
		for _, e := range sa.running {
			b.WriteString(" " + e.Label)
		}
		for _, e := range sa.settling {
			b.WriteString(" " + e.Label)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// fanned builds a sub-agent event as the daemon bridge fans it out : its
// SessionID is the sub-session, tagged with agent_run_id + root_session_id.
func fanned(typ, runID string, seq uint64, payload map[string]any) client.Envelope {
	return client.Envelope{
		Type:          typ,
		SessionID:     "root::agent::" + runID,
		AgentRunID:    runID,
		RootSessionID: "root",
		Seq:           seq,
		Payload:       payload,
	}
}

// TestHandleEnvelope_SubAgentActivityTraced : a delegated sub-agent's own turn
// events, fanned out to the root session, are captured in the sub-agent's own
// activity group — and NEVER spliced into the main streaming bubble or message
// list.
func TestHandleEnvelope_SubAgentActivityTraced(t *testing.T) {
	s := newSubAgentScreen()

	// The coordinator delegates : agent_spawn arrives on the root session
	// (not fanned) and teaches the TUI run_id → kind.
	s.handleEnvelope(client.Envelope{
		Type: "agent_spawn", SessionID: "root", Seq: 1,
		Payload: map[string]any{"run_id": "r1", "kind": "researcher", "child_session_id": "root::agent::r1"},
	})

	// The sub-agent works : its tool call + final message are fanned to root.
	s.handleEnvelope(fanned("tool_call", "r1", 2, map[string]any{"name": "filesystem.read"}))
	s.handleEnvelope(fanned("assistant_message", "r1", 3, map[string]any{"content": "found the answer"}))

	trace := subAgentTrace(s)
	if !strings.Contains(trace, "researcher") {
		t.Fatalf("sub-agent activity not attributed to its kind:\n%s", trace)
	}
	if !strings.Contains(trace, "read") {
		t.Fatalf("sub-agent tool-call not captured in its activity group:\n%s", trace)
	}

	// CRUCIAL : none of the sub-agent text leaked into the coordinator's stream
	// buffer or message list.
	if s.streamBuf != "" {
		t.Errorf("sub-agent text leaked into the main stream buffer: %q", s.streamBuf)
	}
	for _, m := range s.messagesSnapshot() {
		if strings.Contains(m.Content, "found the answer") {
			t.Errorf("sub-agent message leaked into the main message list: %+v", m)
		}
	}
}

// TestHandleEnvelope_SubAgentDoesNotPolluteStream : a normal assistant stream on
// the root session and a concurrent sub-agent fan-out must not cross-contaminate
// — the main bubble keeps only the coordinator's tokens.
func TestHandleEnvelope_SubAgentDoesNotPolluteStream(t *testing.T) {
	s := newSubAgentScreen()
	textParts := func(txt string) map[string]any {
		return map[string]any{"parts": []any{map[string]any{"type": "text", "text": txt}}}
	}

	s.handleEnvelope(client.Envelope{
		Type: "agent_spawn", SessionID: "root", Seq: 1,
		Payload: map[string]any{"run_id": "r1", "kind": "writer"},
	})
	// Coordinator streams "Hel".
	s.handleEnvelope(client.Envelope{Type: "assistant_delta", SessionID: "root", Seq: 2, Payload: textParts("Hel")})
	// Sub-agent emits a delta too (fanned) — must be ignored by the main stream.
	s.handleEnvelope(fanned("assistant_delta", "r1", 3, textParts("SUBAGENT-NOISE")))
	// Coordinator finishes "lo".
	s.handleEnvelope(client.Envelope{Type: "assistant_delta", SessionID: "root", Seq: 4, Payload: textParts("lo")})

	if s.streamBuf != "Hello" {
		t.Fatalf("main stream buffer corrupted by sub-agent delta: %q (want %q)", s.streamBuf, "Hello")
	}
	if strings.Contains(s.streamBuf, "SUBAGENT-NOISE") {
		t.Fatalf("sub-agent delta spliced into coordinator stream: %q", s.streamBuf)
	}
}

// TestHandleEnvelope_ForeignSubAgentDropped : a fan-out tagged for a DIFFERENT
// root session is not ours — it must be dropped, not traced.
func TestHandleEnvelope_ForeignSubAgentDropped(t *testing.T) {
	s := newSubAgentScreen()
	s.handleEnvelope(client.Envelope{
		Type:          "tool_call",
		SessionID:     "other-root::agent::rX",
		AgentRunID:    "rX",
		RootSessionID: "other-root",
		Seq:           1,
		Payload:       map[string]any{"name": "filesystem.read"},
	})
	if len(s.subActivity) != 0 {
		t.Fatalf("foreign sub-agent activity must be dropped, got trace: %s", subAgentTrace(s))
	}
}
