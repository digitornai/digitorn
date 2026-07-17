package hooks_test

import (
	"context"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/hooks"
)

func TestLSPDiagnose_AttachesToToolResult(t *testing.T) {
	fc := &fakeCaller{result: `{"path":"main.go","count":1,"errors":1,"warnings":0,"ok":false,"diagnostics":[{"severity":"error","message":"cannot use string as int"}]}`}
	e := newEngineSync(hooks.LSPDiagnoseHooks(), hooks.ActionDeps{Caller: fc})

	result := map[string]any{"text": "main.go updated (+3 −1)", "status": "completed"}
	res := e.Fire(context.Background(), schema.HookEventToolEnd, nil,
		hooks.Payload{ToolName: "filesystem.edit", ToolArgs: map[string]any{"path": "main.go"}, ToolResult: result})

	if fc.lastTool != "lsp.notify_change" {
		t.Fatalf("called %q, want lsp.notify_change", fc.lastTool)
	}
	if !res.Modified {
		t.Fatal("expected the edit result to be modified with the diagnostics")
	}
	if len(res.Injects) != 0 {
		t.Fatalf("must NOT inject a chat message when it can attach to the result, got %q", res.Injects[0].Content)
	}
	txt, _ := result["text"].(string)
	if !strings.Contains(txt, "main.go updated") || !strings.Contains(txt, "[lsp]") || !strings.Contains(txt, "error") {
		t.Fatalf("diagnostics not folded into the edit result: %q", txt)
	}
}

func TestLSPDiagnose_InjectsWhenNoResultMap(t *testing.T) {
	fc := &fakeCaller{result: `{"path":"main.go","count":1,"errors":1,"warnings":0,"ok":false,"diagnostics":[{"severity":"error","message":"cannot use string as int"}]}`}
	e := newEngineSync(hooks.LSPDiagnoseHooks(), hooks.ActionDeps{Caller: fc})

	res := e.Fire(context.Background(), schema.HookEventToolEnd, nil,
		hooks.Payload{ToolName: "filesystem.edit", ToolArgs: map[string]any{"path": "main.go"}})

	if len(res.Injects) == 0 {
		t.Fatal("no diagnostics injected as fallback — the agent would stay blind")
	}
	if got := res.Injects[0].Content; !strings.Contains(got, "[lsp]") || !strings.Contains(got, "error") {
		t.Fatalf("fallback injection missing the diagnostics: %q", got)
	}
}

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

func TestLSPDiagnose_IgnoresNonEditTools(t *testing.T) {
	fc := &fakeCaller{result: `{"errors":1}`}
	e := newEngineSync(hooks.LSPDiagnoseHooks(), hooks.ActionDeps{Caller: fc})

	e.Fire(context.Background(), schema.HookEventToolEnd, nil,
		hooks.Payload{ToolName: "filesystem.read", ToolArgs: map[string]any{"path": "main.go"}})

	if fc.calls.Load() != 0 {
		t.Fatal("lsp_diagnose fired for a non-edit tool (filesystem.read)")
	}
}
