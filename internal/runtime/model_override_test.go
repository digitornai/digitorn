package runtime_test

import (
	"context"
	"testing"

	"github.com/digitornai/digitorn/internal/llm"
	dgruntime "github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// modelCapturingLLM records the model the engine sent on the request.
type modelCapturingLLM struct{ model string }

func (l *modelCapturingLLM) Chat(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
	l.model = req.Model
	return &llm.ChatResponse{Content: "ok"}, nil
}

// A per-agent model override (EventModelChanged keyed by the agent's logical id)
// replaces that agent Brain's default model for the session's turn.
func TestModelOverride_AppliedToTurn(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	if _, err := sess.AppendDurable(context.Background(), sessionstore.Event{
		Type:      sessionstore.EventModelChanged,
		SessionID: "sess-1",
		Meta:      &sessionstore.MetaPayload{Model: "OVERRIDE-MODEL-XYZ", AgentID: "main"},
	}); err != nil {
		t.Fatalf("append override: %v", err)
	}

	lc := &modelCapturingLLM{}
	e := newEngine(t, apps, sess, lc)
	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if lc.model != "OVERRIDE-MODEL-XYZ" {
		t.Errorf("LLM received model %q, want the agent override", lc.model)
	}
}

// An override keyed by a DIFFERENT agent must not bleed onto the main agent's turn.
func TestModelOverride_OtherAgentUntouched(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-3")

	if _, err := sess.AppendDurable(context.Background(), sessionstore.Event{
		Type:      sessionstore.EventModelChanged,
		SessionID: "sess-3",
		Meta:      &sessionstore.MetaPayload{Model: "SUBAGENT-ONLY", AgentID: "explorer"},
	}); err != nil {
		t.Fatalf("append override: %v", err)
	}

	lc := &modelCapturingLLM{}
	e := newEngine(t, apps, sess, lc)
	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-3", UserID: "u",
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if lc.model == "SUBAGENT-ONLY" {
		t.Errorf("main agent picked up another agent's override %q", lc.model)
	}
}

// Without an override, the LLM gets the agent Brain's declared default model
// (and certainly not a stale override).
func TestModelOverride_DefaultWhenUnset(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-2")

	lc := &modelCapturingLLM{}
	e := newEngine(t, apps, sess, lc)
	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-2", UserID: "u",
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if lc.model == "" || lc.model == "OVERRIDE-MODEL-XYZ" {
		t.Errorf("expected the brain default model, got %q", lc.model)
	}
}
