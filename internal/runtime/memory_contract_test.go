package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime"
)

// TestEngineMemory_YAMLContractGatesTools : memory is OPT-IN per the documented
// contract (docs-site/docs/reference/modules/memory.md — "gated by
// tools.modules.memory"). The memory module is activated like any other module :
// by DECLARING it under tools.modules (presence = enabled, no working_memory
// sub-flag). An app that didn't declare it must NOT be offered the memory tools ;
// one that did must be. Proven end-to-end through the real ContextBuilder →
// wiring → the tool list the LLM actually receives, with the doc-canonical
// memory.* FQN.
func TestEngineMemory_YAMLContractGatesTools(t *testing.T) {
	offeredTools := func(withMemory bool) []llm.ToolSpec {
		app := realDispatchApp()
		if withMemory {
			app.Definition.Tools.Modules = map[string]schema.ModuleBlock{
				"memory": {},
			}
		}
		apps := &stubApps{app: app}
		sess := newProjectingSessions("sess-1")
		lc := &stubLLM{responses: []*llm.ChatResponse{{Content: "done"}}}
		cb, disp := buildRealBus(t, t.TempDir())
		e := newEngine(t, apps, sess, lc)
		e.Context = cb
		e.Dispatcher = disp
		if _, err := e.Run(context.Background(), runtime.TurnInput{
			AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
		}); err != nil {
			t.Fatalf("Run(withMemory=%v): %v", withMemory, err)
		}
		if lc.got == nil {
			t.Fatal("LLM not called")
		}
		return lc.got.Tools
	}
	hasMemoryTool := func(tools []llm.ToolSpec) bool {
		for _, ts := range tools {
			if strings.Contains(ts.Name, "set_goal") || strings.Contains(ts.Name, "remember") ||
				strings.Contains(ts.Name, "task_create") {
				return true
			}
		}
		return false
	}

	if hasMemoryTool(offeredTools(false)) {
		t.Error("app WITHOUT the memory module must NOT be offered memory tools (YAML contract violated)")
	}
	if !hasMemoryTool(offeredTools(true)) {
		t.Error("app that DECLARED tools.modules.memory must be offered the memory tools")
	}
}
