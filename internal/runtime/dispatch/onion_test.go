package dispatch_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/dispatch"
	"github.com/mbathepaul/digitorn/internal/toolmw"
)

type fakeSource struct{ p dispatch.ToolPipeline }

func (s fakeSource) PipelineFor(_, _ string) dispatch.ToolPipeline { return s.p }

// TestBusAdapter_OnionRunsAndIsolatesBySession proves the full daemon-side
// integration : the BusAdapter injects the caller identity into ctx, runs the
// per-app tool-call onion, and the stateful layer (dedup) isolates by session.
func TestBusAdapter_OnionRunsAndIsolatesBySession(t *testing.T) {
	bus := &fakeBus{defaultResult: tool.Result{Success: true, Data: "ok"}}
	pipe := toolmw.Build([]map[string]any{{"dedup": map[string]any{"window_seconds": 60.0}}}, toolmw.Deps{}, nil)
	if pipe == nil {
		t.Fatal("expected a dedup pipeline")
	}
	a := &dispatch.BusAdapter{Bus: bus, Pipelines: fakeSource{p: pipe}}

	call := runtime.ToolInvocation{
		Name: "mod.do", Args: map[string]any{"x": 1},
		AppID: "app", SessionID: "sessA", UserID: "u", AgentID: "main",
	}
	a.Dispatch(context.Background(), call)
	a.Dispatch(context.Background(), call) // identical → dedup collapses to the cached result
	if got := atomic.LoadInt64(&bus.callCount); got != 1 {
		t.Fatalf("dedup must collapse the repeat in the same session, bus hit %d times", got)
	}

	call.SessionID = "sessB" // different session, identical tool+args
	a.Dispatch(context.Background(), call)
	if got := atomic.LoadInt64(&bus.callCount); got != 2 {
		t.Fatalf("a different session must run its own call (isolation), bus hit %d times", got)
	}

	// The caller identity must have reached the module-facing ctx.
	bus.mu.Lock()
	last := bus.calls[len(bus.calls)-1]
	bus.mu.Unlock()
	id, ok := tool.IdentityFromContext(last.Ctx)
	if !ok || id.SessionID != "sessB" || id.ModuleID != "mod" || id.ToolName != "do" || id.AppID != "app" {
		t.Errorf("identity must reach the bus ctx, got %+v ok=%v", id, ok)
	}
}

// TestBusAdapter_NoPipelineFastPath confirms a nil pipeline for the pair runs
// straight through (no onion) but still attaches identity.
func TestBusAdapter_NoPipelineFastPath(t *testing.T) {
	bus := &fakeBus{defaultResult: tool.Result{Success: true, Data: "ok"}}
	a := &dispatch.BusAdapter{Bus: bus, Pipelines: fakeSource{p: nil}}

	out := a.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "mod.do", Args: map[string]any{"x": 1}, AppID: "app", SessionID: "s", UserID: "u",
	})
	if out.Status != "completed" {
		t.Fatalf("expected completed, got %q (%s)", out.Status, out.Error)
	}
	if got := atomic.LoadInt64(&bus.callCount); got != 1 {
		t.Fatalf("fast path must reach the bus exactly once, got %d", got)
	}
	bus.mu.Lock()
	last := bus.calls[len(bus.calls)-1]
	bus.mu.Unlock()
	if id, ok := tool.IdentityFromContext(last.Ctx); !ok || id.SessionID != "s" {
		t.Errorf("identity must be attached even on the fast path, got %+v ok=%v", id, ok)
	}
}
