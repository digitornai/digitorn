package hooks_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/runtime/hooks"
)

// TestLSPDiagnose_InjectsErrors proves the fixed chain: the builtin lsp hook
// fires on a filesystem edit, calls lsp.notify_change, and INJECTS the resulting
// errors into the agent's context (the whole point that was broken before).
func TestLSPDiagnose_InjectsErrors(t *testing.T) {
	fc := &fakeCaller{result: `{"path":"main.go","count":1,"errors":1,"warnings":0,"ok":false,"diagnostics":[{"severity":"error","message":"cannot use string as int"}]}`}
	e := newEngineSync(hooks.LSPDiagnoseHooks(), hooks.ActionDeps{Caller: fc})

	res := e.Fire(context.Background(), schema.HookEventToolEnd, nil,
		hooks.Payload{ToolName: "filesystem.edit", ToolArgs: map[string]any{"path": "main.go"}})

	if fc.calls.Load() == 0 {
		t.Fatal("hook did not call lsp.notify_change")
	}
	if fc.lastTool != "lsp.notify_change" {
		t.Fatalf("called %q, want lsp.notify_change", fc.lastTool)
	}
	if len(res.Injects) == 0 {
		t.Fatal("no diagnostics injected — the agent would stay blind")
	}
	if got := res.Injects[0].Content; !strings.Contains(got, "[lsp]") || !strings.Contains(got, "error") {
		t.Fatalf("injection missing the diagnostics: %q", got)
	}
}

// TestLSPDiagnose_CleanFileNoNoise: a passing file still warms the server but
// injects NOTHING — no noise on every clean edit.
func TestLSPDiagnose_CleanFileNoNoise(t *testing.T) {
	fc := &fakeCaller{result: `{"path":"main.go","count":0,"errors":0,"warnings":0,"ok":true}`}
	e := newEngineSync(hooks.LSPDiagnoseHooks(), hooks.ActionDeps{Caller: fc})

	res := e.Fire(context.Background(), schema.HookEventToolEnd, nil,
		hooks.Payload{ToolName: "filesystem.write", ToolArgs: map[string]any{"path": "main.go"}})

	if fc.calls.Load() == 0 {
		t.Fatal("clean edit should still sync the file to the server")
	}
	if len(res.Injects) != 0 {
		t.Fatalf("clean file must inject nothing, got %q", res.Injects[0].Content)
	}
}

// TestLSPDiagnose_IgnoresNonEditTools: the hook must not fire for reads/searches.
func TestLSPDiagnose_IgnoresNonEditTools(t *testing.T) {
	fc := &fakeCaller{result: `{"errors":1}`}
	e := newEngineSync(hooks.LSPDiagnoseHooks(), hooks.ActionDeps{Caller: fc})

	e.Fire(context.Background(), schema.HookEventToolEnd, nil,
		hooks.Payload{ToolName: "filesystem.read", ToolArgs: map[string]any{"path": "main.go"}})

	if fc.calls.Load() != 0 {
		t.Fatal("lsp_diagnose fired for a non-edit tool (filesystem.read)")
	}
}
