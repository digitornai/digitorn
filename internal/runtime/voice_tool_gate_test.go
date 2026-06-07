package runtime

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// recordingDispatcher records whether Dispatch ran, so a test can prove the gate
// short-circuits BEFORE execution.
type recordingDispatcher struct {
	called bool
	out    ToolOutcome
}

func (r *recordingDispatcher) Dispatch(_ context.Context, _ ToolInvocation) ToolOutcome {
	r.called = true
	return r.out
}

// TestExecuteToolGated_GateBeforeDispatch proves the Voie B (realtime) tool seam : a
// realtime model's function call is gated FIRST (SG-4) and only reaches the
// dispatcher when allowed — the same contract as a tool inside a turn. A gate-blocked
// call must return the gate's errored outcome WITHOUT executing the module.
func TestExecuteToolGated_GateBeforeDispatch(t *testing.T) {
	rec := &recordingDispatcher{out: ToolOutcome{
		Status: "completed",
		Parts:  []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "ran"}},
	}}
	e := &Engine{Dispatcher: rec, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	guard := &modeGuard{
		allowed:     map[string]struct{}{"filesystem.read": {}},
		label:       "Ask",
		allowedList: "filesystem.read",
	}
	ctx := withModeGuard(context.Background(), guard)

	// Blocked tool : returns the gate's errored outcome AND never dispatches.
	out := e.ExecuteToolGated(ctx, ToolInvocation{Name: "filesystem.write", AppID: "a", SessionID: "s"})
	if out.Status != "errored" || !strings.Contains(out.Error, "blocked in mode") {
		t.Fatalf("a gate-blocked tool must return the gate's errored outcome, got %+v", out)
	}
	if rec.called {
		t.Fatal("a gate-blocked tool must NOT reach the dispatcher")
	}

	// Allowed tool : the gate passes → the dispatcher runs and its outcome is returned.
	rec.called = false
	out = e.ExecuteToolGated(ctx, ToolInvocation{Name: "filesystem.read", AppID: "a", SessionID: "s"})
	if !rec.called {
		t.Fatal("an allowed tool must reach the dispatcher")
	}
	if out.Status != "completed" || len(out.Parts) != 1 || out.Parts[0].Text != "ran" {
		t.Fatalf("an allowed tool must return the dispatcher's outcome, got %+v", out)
	}
}
