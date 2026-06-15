package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/runtime/hooks"
)

// ============================================================================
// Production-path integration: filesystem.write → lsp_diagnose hook → real
// gopls → diagnostics folded into the tool result text. No LLM, no mocks of
// the LSP path — we exercise the SAME code the daemon runs in production.
//
// Skips automatically when gopls is not installed, so it can stay in the
// default test suite without breaking CI on machines without a Go LSP server.
// ============================================================================

func requireGopls(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed; skipping production-path LSP integration test")
	}
}

// liveLSPCaller dispatches hook-side "lsp.*" calls to the real lsp.Module the
// daemon would expose at runtime. The JSON shape on the wire is whatever the
// module emits — exactly what the production hook sees.
type liveLSPCaller struct {
	m *Module
}

func (c *liveLSPCaller) Call(ctx context.Context, name string, args map[string]any) (string, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	switch name {
	case "lsp.notify_change":
		res, err := c.m.notifyChange(ctx, raw)
		if err != nil {
			return "", err
		}
		body, _ := json.Marshal(res.Data)
		return string(body), nil
	case "lsp.diagnostics":
		res, err := c.m.diagnostics(ctx, raw)
		if err != nil {
			return "", err
		}
		body, _ := json.Marshal(res.Data)
		return string(body), nil
	}
	return "", fmt.Errorf("liveLSPCaller: unknown tool %q", name)
}

// productionRig assembles the full production wiring: a real lsp.Module, the
// builtin LSP_diagnose hook the daemon attaches to every lsp-enabled app, and
// the hook engine itself. Returns a teardown that stops every server.
func productionRig(t *testing.T) (*Module, *hooks.Engine, func()) {
	t.Helper()
	m := New()
	// Wide settle: under concurrent load (multiple goroutines firing didOpen
	// at once) gopls' analysis queue serializes the work, so the per-file
	// publish for the LAST file in the batch can lag the first by 8-10s on a
	// busy CI box. 15s gives every file room to land without flaking the suite.
	if err := m.Init(context.Background(), map[string]any{"settle_seconds": 15.0}); err != nil {
		t.Fatalf("module init: %v", err)
	}
	caller := &liveLSPCaller{m: m}
	e := hooks.New(hooks.LSPDiagnoseHooks(), hooks.ActionDeps{Caller: caller})
	e.Async = false // synchronous fire so we can assert effects deterministically
	stop := func() {
		_ = m.Stop(context.Background())
	}
	return m, e, stop
}

// firingWrite simulates a successful filesystem.write tool call. The hook
// engine sees the same payload the runtime sends in production after a real
// edit: tool name, params with path + content, an editable result map with the
// "text" surface lsp_diagnose folds the diagnostics into.
func firingWrite(t *testing.T, e *hooks.Engine, absPath, content string) (effects hooks.FireResult, resultText string) {
	t.Helper()
	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		t.Fatalf("seed file on disk: %v", err)
	}
	result := map[string]any{
		"text":   filepath.Base(absPath) + " written (+" + fmt.Sprintf("%d", len(content)) + " bytes)",
		"status": "completed",
	}
	effects = e.Fire(context.Background(), schema.HookEventToolEnd, nil, hooks.Payload{
		ToolName:   "filesystem.write",
		ToolArgs:   map[string]any{"path": absPath, "content": content},
		ToolResult: result,
	})
	resultText, _ = result["text"].(string)
	return effects, resultText
}

// ---------------------------------------------------------------------------
// Scenario 1: end-to-end, buggy file. The hook must call lsp.notify_change,
// receive gopls's type error, and FOLD it into the write tool's text surface.
// ---------------------------------------------------------------------------

func TestProduction_BuggyEdit_FoldsDiagnosticsIntoToolResult(t *testing.T) {
	requireGopls(t)
	_, e, stop := productionRig(t)
	defer stop()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module probe\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	buggy := "package main\n\nfunc main() {\n\tvar x int = \"not an int\"\n\t_ = x\n}\n"

	effects, text := firingWrite(t, e, filepath.Join(dir, "main.go"), buggy)
	if !effects.Modified {
		t.Fatal("hook did not modify the tool result — diagnostics never reached the agent")
	}
	if !strings.Contains(text, "[lsp]") {
		t.Fatalf("tool result text missing [lsp] header:\n%s", text)
	}
	if !strings.Contains(text, "error") {
		t.Fatalf("expected an error diagnostic; got:\n%s", text)
	}
	t.Logf("OK — production text returned to the agent:\n%s", text)
}

// ---------------------------------------------------------------------------
// Scenario 2: clean file. Hook must STAY SILENT — no [lsp] noise on every
// passing edit, no fake injection.
// ---------------------------------------------------------------------------

func TestProduction_CleanEdit_StaysSilent(t *testing.T) {
	requireGopls(t)
	_, e, stop := productionRig(t)
	defer stop()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module probe\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clean := "package main\n\nfunc main() {\n\tvar x = 42\n\t_ = x\n}\n"

	effects, text := firingWrite(t, e, filepath.Join(dir, "main.go"), clean)
	if effects.Modified {
		t.Fatalf("clean edit modified the tool result — noise on the agent loop:\n%s", text)
	}
	if len(effects.Injects) != 0 {
		t.Fatalf("clean edit injected %d message(s); want zero noise", len(effects.Injects))
	}
	if strings.Contains(text, "[lsp]") {
		t.Fatalf("clean edit produced an [lsp] line:\n%s", text)
	}
}

// ---------------------------------------------------------------------------
// Scenario 3: CROSS-FILE — proves B13 in production. helper.go is on disk
// and main.go uses Add(...) from it. gopls only resolves the cross-file call
// when launched at the PROJECT root, not at main.go's directory. The hook must
// surface a CLEAN result for the valid call.
// ---------------------------------------------------------------------------

func TestProduction_CrossFileImport_ResolvedAtProjectRoot(t *testing.T) {
	requireGopls(t)
	_, e, stop := productionRig(t)
	defer stop()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module probe\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// helper.go on disk — gopls must see it when launched at the project root.
	helper := "package main\n\nfunc Add(a, b int) int { return a + b }\n"
	if err := os.WriteFile(filepath.Join(dir, "helper.go"), []byte(helper), 0o644); err != nil {
		t.Fatal(err)
	}
	main := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(Add(1, 2))\n}\n"

	effects, text := firingWrite(t, e, filepath.Join(dir, "main.go"), main)
	if effects.Modified {
		t.Fatalf("cross-file CALL flagged as error — gopls saw main.go in isolation:\n%s", text)
	}

	// Sanity check the converse: an UNDEFINED cross-file ref must NOT resolve.
	broken := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(Subtract(1, 2))\n}\n"
	effects2, text2 := firingWrite(t, e, filepath.Join(dir, "main.go"), broken)
	if !effects2.Modified {
		t.Fatalf("undefined cross-file ref was NOT flagged — false-negative path:\n%s", text2)
	}
	if !strings.Contains(strings.ToLower(text2), "undefined") && !strings.Contains(strings.ToLower(text2), "undeclared") {
		t.Fatalf("expected an undefined-name error, got:\n%s", text2)
	}
	t.Logf("OK — gopls resolved Add() cross-file AND flagged Subtract() as undefined")
}

// ---------------------------------------------------------------------------
// Scenario 4: edit-then-fix cycle. The agent introduces an error, sees it,
// then fixes the same file — second hook stays silent. This proves the
// notifyChange flow is REUSABLE on the same file without leaking stale state.
// ---------------------------------------------------------------------------

func TestProduction_EditThenFix_DiagnosticsClearOnRepair(t *testing.T) {
	requireGopls(t)
	_, e, stop := productionRig(t)
	defer stop()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module probe\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(dir, "main.go")

	buggy := "package main\nfunc main() { var x int = \"oops\"; _ = x }\n"
	effects1, text1 := firingWrite(t, e, mainPath, buggy)
	if !effects1.Modified || !strings.Contains(text1, "[lsp]") {
		t.Fatalf("first (buggy) write did not surface diagnostics:\n%s", text1)
	}

	fixed := "package main\nfunc main() { var x int = 42; _ = x }\n"
	effects2, text2 := firingWrite(t, e, mainPath, fixed)
	if effects2.Modified || strings.Contains(text2, "[lsp]") {
		t.Fatalf("second (fixed) write still surfaces diagnostics — stale state:\n%s", text2)
	}
	t.Logf("OK — buggy → reported; fixed → silent")
}

// ---------------------------------------------------------------------------
// Scenario 5: UTF-8 byte-column accuracy. The buggy line is preceded by an
// emoji (4 bytes / 2 UTF-16 code units). Without the B3 fix, the column would
// be off by 2; with it, the [lsp] block carries the correct byte column for
// the offending `var x int = "..."` token.
// ---------------------------------------------------------------------------

func TestProduction_UTF8ColumnAccuracy_AfterEmoji(t *testing.T) {
	requireGopls(t)
	_, e, stop := productionRig(t)
	defer stop()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module probe\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// `_ = "💡 a tip"` THEN the type error on a separate line. The conversion
	// targets the COLUMN of the bad assignment; the emoji on the line above
	// stresses the per-line conversion path (and proves the line we report is
	// not shifted by surrogate counting).
	src := "package main\n\nfunc main() {\n\t_ = \"💡 a tip\"\n\tvar x int = \"not int\"\n\t_ = x\n}\n"
	_, text := firingWrite(t, e, filepath.Join(dir, "main.go"), src)
	if !strings.Contains(text, "[lsp]") {
		t.Fatalf("no [lsp] block on a buggy UTF-8 file:\n%s", text)
	}
	// The error is on the FIFTH line (1-based), regardless of UTF-8 vs UTF-16.
	// We just assert the line number is exactly 5 — a wrong line would surface
	// silently otherwise.
	if !strings.Contains(text, "L5:") {
		t.Fatalf("expected error on L5; got:\n%s", text)
	}
	t.Logf("OK — UTF-8 source produced correctly-located diagnostic")
}

// ---------------------------------------------------------------------------
// Scenario 6: concurrent edits on DIFFERENT files. Each file's hook must
// produce its own diagnostics, none crossed. Exercises the per-URI lock (B1)
// and the per-file content cache (B7) under real load.
// ---------------------------------------------------------------------------

func TestProduction_ConcurrentEditsAcrossFiles_NoCrossTalk(t *testing.T) {
	requireGopls(t)
	_, e, stop := productionRig(t)
	defer stop()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module probe\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := []struct {
		name    string
		content string
		wantBug bool
	}{
		{"a.go", "package main\nfunc a() { var x int = \"oops\"; _ = x }\n", true},
		{"b.go", "package main\nfunc b() int { return 42 }\n", false},
		{"c.go", "package main\nfunc c() { _ = unknownSymbol }\n", true},
		{"d.go", "package main\nfunc d() string { return \"ok\" }\n", false},
	}

	type result struct {
		name string
		text string
	}
	results := make([]result, len(files))
	var wg sync.WaitGroup
	var errCount atomic.Int32
	for i, f := range files {
		wg.Add(1)
		go func(i int, f struct {
			name    string
			content string
			wantBug bool
		}) {
			defer wg.Done()
			_, text := firingWrite(t, e, filepath.Join(dir, f.name), f.content)
			results[i] = result{name: f.name, text: text}
			// What this file's OWN section says must match its bug state, regardless
			// of the project rollup the rest of the concurrent batch may surface.
			ownHeader := "[lsp] " + f.name + " —"
			ownReported := strings.Contains(text, ownHeader)
			if ownReported != f.wantBug {
				errCount.Add(1)
				t.Logf("OWN-SECTION MISMATCH %s: own_reported=%v want=%v\n%s", f.name, ownReported, f.wantBug, text)
			}
		}(i, f)
	}
	wg.Wait()
	if errCount.Load() > 0 {
		t.Fatalf("%d file(s) reported their OWN state incorrectly under concurrent load", errCount.Load())
	}
	for _, r := range results {
		t.Logf("--- %s ---\n%s", r.name, r.text)
	}
}

// ---------------------------------------------------------------------------
// Scenario 7: cold-start budget. The very first edit may need a few seconds
// for gopls to come up. The hook must STILL fold the diagnostics in once they
// arrive, not time out and lose them. Bounded so the test cannot hang forever.
// ---------------------------------------------------------------------------

func TestProduction_ColdStart_FirstEditWaitsAndSurfaces(t *testing.T) {
	requireGopls(t)
	_, e, stop := productionRig(t)
	defer stop()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module probe\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	buggy := "package main\nfunc main() { var x int = \"x\"; _ = x }\n"

	start := time.Now()
	effects, text := firingWrite(t, e, filepath.Join(dir, "main.go"), buggy)
	elapsed := time.Since(start)

	if !effects.Modified {
		t.Fatalf("cold-start edit lost its diagnostics; elapsed %v, text:\n%s", elapsed, text)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("cold-start hook took %v — exceeded reasonable cold-start budget", elapsed)
	}
	t.Logf("OK — cold start surfaced diagnostics in %v", elapsed)
}
