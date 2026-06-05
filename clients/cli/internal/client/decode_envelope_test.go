package client

import "testing"

// TestDecodeEnvelope_CorrelationAndAgentFields : the socket.io payload (a
// map[string]any post-JSON) must decode the correlation_id (used to tie a
// run_parallel child's tool_progress to its parent chip) and the agent
// fan-out fields. These are the wire keys the daemon emits — if decode drops
// them, the typed Envelope silently loses them and the TUI can't route.
func TestDecodeEnvelope_CorrelationAndAgentFields(t *testing.T) {
	raw := map[string]any{
		"type":            "tool_progress",
		"seq":             float64(12), // JSON numbers are float64
		"session_id":      "s1",
		"correlation_id":  "parent-call",
		"agent_run_id":    "r1",
		"root_session_id": "root",
		"payload": map[string]any{
			"name":     "filesystem.read",
			"status":   "completed",
			"metadata": map[string]any{"completed": float64(2), "total": float64(4)},
		},
	}

	env := decodeEnvelope(raw)

	if env.Type != "tool_progress" {
		t.Errorf("type: got %q", env.Type)
	}
	if env.Seq != 12 {
		t.Errorf("seq: got %d", env.Seq)
	}
	if env.CorrelationID != "parent-call" {
		t.Errorf("correlation_id dropped by decode: got %q", env.CorrelationID)
	}
	if env.AgentRunID != "r1" {
		t.Errorf("agent_run_id dropped by decode: got %q", env.AgentRunID)
	}
	if env.RootSessionID != "root" {
		t.Errorf("root_session_id dropped by decode: got %q", env.RootSessionID)
	}
	md, _ := env.Payload["metadata"].(map[string]any)
	if md == nil || md["completed"] != float64(2) || md["total"] != float64(4) {
		t.Errorf("payload metadata not carried through: %+v", env.Payload)
	}
}
