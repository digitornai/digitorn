package toolmw

import (
	"context"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
)

// TestTimeout_ModulePanicNeverCrashes proves a module that panics on the
// per-call timeout goroutine surfaces as ONE errored result instead of taking
// the daemon down. The goroutine runs outside the MetaDispatcher recover, so
// without its own shield this panic would crash the process.
func TestTimeout_ModulePanicNeverCrashes(t *testing.T) {
	mw, err := newTimeout(map[string]any{"seconds": 5.0}, Deps{})
	if err != nil {
		t.Fatal(err)
	}

	var panicking Next = func(ctx context.Context, cc CallContext) (tool.Result, error) {
		panic("module exploded")
	}

	res, err := mw.Handle(context.Background(),
		CallContext{ModuleID: "boom", ToolName: "go"}, panicking)

	if err == nil {
		t.Fatal("a recovered module panic must surface as an error")
	}
	if res.Success {
		t.Fatalf("a recovered module panic must be an errored result, got %+v", res)
	}
	if !strings.Contains(res.Error, "panic recovered") {
		t.Fatalf("error should explain the recovered panic, got %q", res.Error)
	}

	// The middleware is still usable afterwards — the shield did not break it.
	ok, _ := mw.Handle(context.Background(), CallContext{ModuleID: "ok", ToolName: "go"},
		func(ctx context.Context, cc CallContext) (tool.Result, error) {
			return tool.Result{Success: true}, nil
		})
	if !ok.Success {
		t.Fatal("middleware unusable after a recovered panic")
	}
}
