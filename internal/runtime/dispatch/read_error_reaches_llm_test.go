package dispatch_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/adapter"
	"github.com/mbathepaul/digitorn/internal/runtime/dispatch"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// TestReadError_ReachesLLM proves the claim under investigation : when
// filesystem.read fails (e.g. an absolute path rejected by the workspace
// guard), does the error reach the model, or is it swallowed into an empty
// result?
//
// It walks the SAME path a live read takes :
//
//	module errResult (Success=false, Error)  →  BusAdapter.Dispatch
//	→ sessionstore projection (Tool.Error → ToolResultSpec.Error)
//	→ adapter.MessagesToLLM (the bytes the model actually sees)
//
// If the model's tool message contains the error text, the pipeline is sound
// and the live "agent didn't get the error" is something else (path handling).
// If it is empty, this test fails loudly and we have found the swallow.
func TestReadError_ReachesLLM(t *testing.T) {
	const errMsg = `path "C:\x\buggy.go" must be relative to the workspace`

	// 1) Module layer : a bad read returns errResult → Success=false + Error.
	bus := &fakeBus{defaultResult: tool.Result{Success: false, Error: errMsg}}
	a := dispatch.NewBusAdapter(bus)

	out := a.Dispatch(context.Background(), runtime.ToolInvocation{
		Name: "filesystem.read",
		Args: map[string]any{"path": `C:\x\buggy.go`},
	})
	if out.Status != "errored" {
		t.Fatalf("dispatch: status = %q, want errored", out.Status)
	}
	if !strings.Contains(out.Error, "must be relative to the workspace") {
		t.Fatalf("dispatch SWALLOWED the error: ToolOutcome.Error = %q", out.Error)
	}

	// 2) Projection : mimic engine.persistToolResults → the durable tool message
	//    (Tool.Error → ToolResultSpec.Error, sessionstore/projection.go:135).
	msgs := []sessionstore.Message{{
		Role: "tool",
		Parts: []sessionstore.MessagePart{{
			Type: sessionstore.PartTypeToolResult,
			ToolResult: &sessionstore.ToolResultSpec{
				ToolCallID: "call-1",
				Error:      out.Error,
			},
		}},
	}}

	// 3) LLM-facing adapter : exactly the bytes the model receives next turn.
	llmMsgs := adapter.MessagesToLLM(context.Background(), msgs, adapter.Options{})
	if len(llmMsgs) != 1 {
		t.Fatalf("adapter: got %d messages, want 1 (orphan dropped?)", len(llmMsgs))
	}

	got := llmMsgs[0]
	seen := got.Content
	for _, p := range got.Parts {
		seen += p.Text
	}
	if !strings.Contains(seen, "must be relative to the workspace") {
		t.Fatalf("BUG CONFIRMED — the model does NOT receive the read error. content=%q", seen)
	}
	t.Logf("PROVEN : the model receives the read error verbatim → %q", seen)
}
