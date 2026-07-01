package meta_test

import (
	"context"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/context/meta"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

type okInner struct{}

func (okInner) Dispatch(_ context.Context, _ runtime.ToolInvocation) runtime.ToolOutcome {
	return runtime.ToolOutcome{Status: "completed"}
}

// TestRunParallel_EmitsPerChildProgress : run_parallel must emit ONE
// EventToolProgress per action as it finishes (so the client updates
// incrementally) — for ANY tool, generically — while the agent still gets the
// single combined barrier result. The Progress callback fires from the fan-in,
// not per goroutine, so it's serial and covers every child.
func TestRunParallel_EmitsPerChildProgress(t *testing.T) {
	var progress []sessionstore.Event
	d := &meta.MetaDispatcher{
		Inner: okInner{},
		Progress: func(_ context.Context, ev sessionstore.Event) {
			progress = append(progress, ev)
		},
	}
	out := d.Dispatch(context.Background(), runtime.ToolInvocation{
		Name:      "context_builder.run_parallel",
		AppID:     "app",
		AgentID:   "main",
		SessionID: "s",
		CallID:    "c1",
		Args: map[string]any{"tasks": []any{
			map[string]any{"tool": "fs.read", "args": map[string]any{"path": "a"}},
			map[string]any{"tool": "fs.read", "args": map[string]any{"path": "b"}},
			map[string]any{"tool": "fs.write", "args": map[string]any{"path": "c"}},
		}},
	})

	// One progress event per child, all of the transient type.
	if len(progress) != 3 {
		t.Fatalf("want 3 per-child progress events, got %d", len(progress))
	}
	names := map[string]int{}
	for _, ev := range progress {
		if ev.Type != sessionstore.EventToolProgress {
			t.Errorf("progress event has wrong type %q", ev.Type)
		}
		if ev.Tool == nil {
			t.Errorf("progress event missing tool payload: %+v", ev)
			continue
		}
		if ev.CorrelationID != "c1" {
			t.Errorf("progress must tie to the parent call_id, got %q", ev.CorrelationID)
		}
		names[ev.Tool.Name]++
	}
	if names["fs.read"] != 2 || names["fs.write"] != 1 {
		t.Errorf("per-child names wrong: %v", names)
	}

	// The agent's barrier result is unchanged — still one combined envelope.
	var text string
	for _, p := range out.Parts {
		text += p.Text
	}
	if !strings.Contains(text, "results") {
		t.Errorf("barrier result must still carry the combined results envelope, got %q", text)
	}
}
