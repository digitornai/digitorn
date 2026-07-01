package server

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/runtime/agent"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// TestAgentLifecycle_ReachesParentStream locks the FULL real-time path a
// connected client depends on for sub-agent visibility —
//
//	Manager.emit → Bus.AppendDurable → SubscribeAll → bridge.dispatchToRealtime
//	→ realtime.Emit(room="session:<root>")
//
// and asserts the bridge emits BOTH an agent_spawn and an agent_result envelope
// into the root session's room, each carrying the AgentPayload. This path had no
// coverage before. NOTE the scope : this is the agent LIFECYCLE (spawn/result).
// The sub-agent's OWN turn activity (assistant tokens, its tool calls) is emitted
// to the isolated sub-session "root::agent::<runID>" and is NOT surfaced on the
// parent stream — by design today ; see the sub-agent observability work.
func TestAgentLifecycle_ReachesParentStream(t *testing.T) {
	_, bus, rt, _, cleanup := setupBridge(t)
	defer cleanup()

	const root = "sess-root-diag"
	wantRoom := "session:" + root

	am := agent.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	am.AttachSink(bus) // exactly like bootstrap : durable lifecycle events on the bus
	am.AttachRunner(resyncStubRunner{toolCalls: 2, tokensIn: 40, tokensOut: 9})

	runID, err := am.Spawn(context.Background(), agent.SpawnRequest{
		AppID: "app-1", RootSession: root, UserID: "user-A", AgentID: "researcher", Task: "dig",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if _, err := am.Wait(context.Background(), root, runID, 2*time.Second); err != nil {
		t.Fatalf("wait: %v", err)
	}

	// Collect the bridge's emits to the root room, by event type.
	var sawSpawn, sawResult bool
	var resultPayload *sessionstore.AgentPayload
	waitUntil(t, func() bool {
		for _, e := range rt.recordedEmits() {
			if e.Room != wantRoom || e.Event != "event" {
				continue
			}
			env, ok := e.Data.(sessionstore.SocketEnvelope)
			if !ok {
				continue
			}
			switch env.Type {
			case string(sessionstore.EventAgentSpawn):
				sawSpawn = true
			case string(sessionstore.EventAgentResult):
				sawResult = true
				if ap, ok := env.Payload.(*sessionstore.AgentPayload); ok {
					resultPayload = ap
				}
			}
		}
		return sawSpawn && sawResult
	}, "bridge emits agent_spawn + agent_result to session:<root>")

	if !sawSpawn {
		t.Error("agent_spawn never emitted to the parent (root) session stream")
	}
	if !sawResult {
		t.Error("agent_result never emitted to the parent (root) session stream")
	}
	if resultPayload == nil {
		t.Fatal("agent_result envelope carried no AgentPayload")
	}
	if resultPayload.RunID != runID {
		t.Errorf("agent_result payload run_id = %q, want %q", resultPayload.RunID, runID)
	}
	if resultPayload.Status != "completed" {
		t.Errorf("agent_result payload status = %q, want completed", resultPayload.Status)
	}
}
