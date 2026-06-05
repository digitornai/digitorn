package server

import (
	"context"
	"testing"

	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// emitsToRoom collects the bridge's recorded emits to a given room, decoded as
// envelopes.
func emitsToRoom(rt *fakeRealtime, room string) []sessionstore.SocketEnvelope {
	var out []sessionstore.SocketEnvelope
	for _, e := range rt.recordedEmits() {
		if e.Room != room || e.Event != "event" {
			continue
		}
		if env, ok := e.Data.(sessionstore.SocketEnvelope); ok {
			out = append(out, env)
		}
	}
	return out
}

func subAgentMsg(session string) sessionstore.Event {
	return sessionstore.Event{
		Type:      sessionstore.EventAssistantMessage,
		SessionID: session,
		AppID:     "app-1",
		UserID:    "user-A",
		Message: &sessionstore.MessagePayload{
			Role:  "assistant",
			Parts: []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "sub-agent at work"}},
		},
	}
}

// TestBridge_SubAgentActivityFannedToRoot : the headline of this feature. A
// sub-agent's OWN turn event, emitted to its isolated sub-session, must ALSO
// reach the root session's room (so a client watching the top-level session
// sees the work live), tagged with the emitting agent's run id — while STILL
// being delivered to the sub-session's own room (additive, nothing removed).
func TestBridge_SubAgentActivityFannedToRoot(t *testing.T) {
	_, bus, rt, _, cleanup := setupBridge(t)
	defer cleanup()

	const root = "sess-root"
	const sub = root + "::agent::run-1"

	if _, err := bus.AppendDurable(context.Background(), subAgentMsg(sub)); err != nil {
		t.Fatalf("append: %v", err)
	}

	waitUntil(t, func() bool {
		return len(emitsToRoom(rt, "session:"+root)) > 0 &&
			len(emitsToRoom(rt, "session:"+sub)) > 0
	}, "event reaches BOTH the sub-session room and the root room")

	// Primary route : the sub-session's own room, untagged.
	prim := emitsToRoom(rt, "session:"+sub)
	if len(prim) != 1 {
		t.Fatalf("expected 1 primary emit to the sub-session room, got %d", len(prim))
	}
	if prim[0].AgentRunID != "" || prim[0].RootSessionID != "" {
		t.Errorf("primary (sub-session) emit must NOT carry fan-out tags: %+v", prim[0])
	}

	// Fan-out route : the root room, tagged for attribution.
	fan := emitsToRoom(rt, "session:"+root)
	if len(fan) != 1 {
		t.Fatalf("expected 1 fan-out emit to the root room, got %d", len(fan))
	}
	if fan[0].AgentRunID != "run-1" {
		t.Errorf("fan-out agent_run_id = %q, want run-1", fan[0].AgentRunID)
	}
	if fan[0].RootSessionID != root {
		t.Errorf("fan-out root_session_id = %q, want %q", fan[0].RootSessionID, root)
	}
	// The original sub-session id is preserved on the envelope so the client can
	// still tell which exact (possibly nested) session produced it.
	if fan[0].SessionID != sub {
		t.Errorf("fan-out envelope session_id = %q, want the sub-session %q", fan[0].SessionID, sub)
	}
}

// TestBridge_NestedSubAgentFansToTopRoot : a deeply nested sub-agent
// (root::agent::A::agent::B) streams to the TOP-LEVEL root room, attributed to
// the deepest (emitting) agent B — so one root subscription sees the whole tree.
func TestBridge_NestedSubAgentFansToTopRoot(t *testing.T) {
	_, bus, rt, _, cleanup := setupBridge(t)
	defer cleanup()

	const root = "sess-root"
	const sub = root + "::agent::run-A::agent::run-B"

	if _, err := bus.AppendDurable(context.Background(), subAgentMsg(sub)); err != nil {
		t.Fatalf("append: %v", err)
	}

	waitUntil(t, func() bool {
		return len(emitsToRoom(rt, "session:"+root)) > 0
	}, "nested sub-agent event reaches the top-level root room")

	fan := emitsToRoom(rt, "session:"+root)
	if len(fan) != 1 {
		t.Fatalf("expected 1 fan-out emit to the top-level root room, got %d", len(fan))
	}
	if fan[0].AgentRunID != "run-B" {
		t.Errorf("nested fan-out must attribute to the emitting (deepest) agent: run_id = %q, want run-B", fan[0].AgentRunID)
	}
	if fan[0].RootSessionID != root {
		t.Errorf("nested fan-out root_session_id = %q, want %q", fan[0].RootSessionID, root)
	}
}

// TestBridge_PlainSessionNotFannedOut : a normal (non-sub-agent) event is routed
// ONLY to its own session room, never duplicated or tagged. Guards against the
// fan-out firing for ordinary traffic.
func TestBridge_PlainSessionNotFannedOut(t *testing.T) {
	_, bus, rt, _, cleanup := setupBridge(t)
	defer cleanup()

	const sess = "plain-session"
	if _, err := bus.AppendDurable(context.Background(), subAgentMsg(sess)); err != nil {
		t.Fatalf("append: %v", err)
	}

	waitUntil(t, func() bool {
		return len(emitsToRoom(rt, "session:"+sess)) > 0
	}, "plain event reaches its own session room")

	got := emitsToRoom(rt, "session:"+sess)
	if len(got) != 1 {
		t.Fatalf("plain session must get exactly 1 emit, got %d (fan-out leaked?)", len(got))
	}
	if got[0].AgentRunID != "" || got[0].RootSessionID != "" {
		t.Errorf("plain event must carry no fan-out tags: %+v", got[0])
	}
	// And the bridge made no other session-room emits at all.
	total := 0
	for _, e := range rt.recordedEmits() {
		if e.Event == "event" {
			total++
		}
	}
	if total != 1 {
		t.Errorf("plain event produced %d emits, want exactly 1 (no spurious fan-out)", total)
	}
}
