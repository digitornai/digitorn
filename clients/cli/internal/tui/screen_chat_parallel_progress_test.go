package tui

import (
	"testing"

	"github.com/digitornai/digitorn-cli/internal/client"
)

func parallelChip(s *ChatScreen, callID string) (client.Message, bool) {
	for _, m := range s.messagesSnapshot() {
		if m.Role == "tool" && m.CallID == callID {
			return m, true
		}
	}
	return client.Message{}, false
}

// TestHandleEnvelope_ParallelProgressAdvancesChip : a run_parallel child's
// tool_progress (tied to the parent via correlation_id) advances the parent
// chip's live "N/total done" hint in place — before the combined result lands.
// Metadata arrives as float64 (post-JSON), the parent chip count stays 1, and
// a stray progress for an unknown parent is a no-op (never spawns a chip).
func TestHandleEnvelope_ParallelProgressAdvancesChip(t *testing.T) {
	s := newSubAgentScreen()

	// The agent launches run_parallel : its chip is created "running".
	s.handleEnvelope(client.Envelope{
		Type: "tool_call", SessionID: "root", Seq: 1,
		Payload: map[string]any{"name": "context_builder.run_parallel", "call_id": "c1"},
	})
	if _, ok := parallelChip(s, "c1"); !ok {
		t.Fatalf("run_parallel chip not created")
	}

	// First child finishes : 1 of 4.
	s.handleEnvelope(client.Envelope{
		Type: "tool_progress", SessionID: "root", Seq: 2, CorrelationID: "c1",
		Payload: map[string]any{
			"name": "filesystem.read", "status": "completed",
			"metadata": map[string]any{"completed": float64(1), "total": float64(4)},
		},
	})
	chip, _ := parallelChip(s, "c1")
	if chip.ToolArg != "1/4 done" {
		t.Fatalf("after first child want ToolArg %q, got %q", "1/4 done", chip.ToolArg)
	}

	// Second child finishes : the SAME chip advances to 2/4 (update in place).
	s.handleEnvelope(client.Envelope{
		Type: "tool_progress", SessionID: "root", Seq: 3, CorrelationID: "c1",
		Payload: map[string]any{
			"name": "filesystem.write", "status": "completed",
			"metadata": map[string]any{"completed": float64(2), "total": float64(4)},
		},
	})
	chip, _ = parallelChip(s, "c1")
	if chip.ToolArg != "2/4 done" {
		t.Fatalf("after second child want ToolArg %q, got %q", "2/4 done", chip.ToolArg)
	}

	// Exactly one tool chip exists — progress never appends.
	tools := 0
	for _, m := range s.messagesSnapshot() {
		if m.Role == "tool" {
			tools++
		}
	}
	if tools != 1 {
		t.Fatalf("progress events must update in place, want 1 tool chip, got %d", tools)
	}

	// A progress event for a parent we don't know is a silent no-op.
	before := len(s.messagesSnapshot())
	s.handleEnvelope(client.Envelope{
		Type: "tool_progress", SessionID: "root", Seq: 4, CorrelationID: "ghost",
		Payload: map[string]any{
			"name": "filesystem.read", "status": "completed",
			"metadata": map[string]any{"completed": float64(1), "total": float64(2)},
		},
	})
	if after := len(s.messagesSnapshot()); after != before {
		t.Fatalf("unknown-parent progress must not create a chip: %d -> %d", before, after)
	}
}
