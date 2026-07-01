package lsp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/runtime/hooks"
)

// =============================================================================
// Validator-level unit tests — pure Go, no subprocess, no install needed.
// =============================================================================

func TestBuiltin_JSON(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"empty", "", false},
		{"valid object", `{"a":1,"b":[2,3]}`, false},
		{"valid array", `[1,2,3]`, false},
		{"trailing comma", `{"a":1,}`, true},
		{"unterminated string", `{"a":"x}`, true},
		{"bad token", `{a:1}`, true},
		{"empty object", `{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validateJSON(tc.body)
			if tc.wantErr && len(d) == 0 {
				t.Fatalf("expected error, got none")
			}
			if !tc.wantErr && len(d) != 0 {
				t.Fatalf("expected no error, got %+v", d)
			}
			if tc.wantErr {
				if d[0].Severity != "error" || d[0].Source != "json" {
					t.Errorf("wrong diag shape: %+v", d[0])
				}
				if d[0].Line < 1 {
					t.Errorf("line should be 1-based, got %d", d[0].Line)
				}
			}
		})
	}
}

func TestBuiltin_YAML(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"empty", "", false},
		{"simple", "a: 1\nb: 2\n", false},
		{"list", "- a\n- b\n", false},
		{"duplicate key", "a:\n  b: 1\n  b: 2\n", true},
		{"unterminated", `a: "hello`, true},
		{"multi-doc valid", "a: 1\n---\nb: 2\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validateYAML(tc.body)
			if tc.wantErr && len(d) == 0 {
				t.Fatalf("expected error, got none")
			}
			if !tc.wantErr && len(d) != 0 {
				t.Fatalf("expected no error, got %+v", d)
			}
		})
	}
}

func TestBuiltin_XML(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"empty", "", false},
		{"valid", `<root><a>1</a><b>2</b></root>`, false},
		{"mismatched close", `<root><a></b></root>`, true},
		{"unclosed", `<root><a>`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validateXML(tc.body)
			if tc.wantErr && len(d) == 0 {
				t.Fatalf("expected error, got none")
			}
			if !tc.wantErr && len(d) != 0 {
				t.Fatalf("expected no error, got %+v", d)
			}
		})
	}
}

func TestBuiltin_HTML(t *testing.T) {
	if d := validateHTML("<!DOCTYPE html><html><body><p>ok</p></body></html>"); len(d) != 0 {
		t.Errorf("valid HTML flagged: %+v", d)
	}
	if d := validateHTML(""); len(d) != 0 {
		t.Errorf("empty content flagged: %+v", d)
	}
}

// =============================================================================
// End-to-end through the SAME production hook pipeline a real agent uses —
// no gopls / pyright required, the builtin validator is pure Go.
// =============================================================================

// liteRig wires the lsp module + lsp_diagnose hook engine WITHOUT requiring
// any external language server: every spec exercised here uses the builtin
// protocol, so the rig boots instantly and works on any machine.
func liteRig(t *testing.T) (*Module, *hooks.Engine, func()) {
	t.Helper()
	m := New()
	if err := m.Init(context.Background(), map[string]any{"settle_seconds": 2.0}); err != nil {
		t.Fatalf("module init: %v", err)
	}
	caller := &liveLSPCaller{m: m}
	e := hooks.New(hooks.LSPDiagnoseHooks(), hooks.ActionDeps{Caller: caller})
	e.Async = false
	stop := func() { _ = m.Stop(context.Background()) }
	return m, e, stop
}

func TestBuiltin_E2E_JSONErrorFoldedIntoToolResult(t *testing.T) {
	_, e, stop := liteRig(t)
	defer stop()

	dir := t.TempDir()
	ef, text := firingWrite(t, e, filepath.Join(dir, "config.json"), `{"a":1,}`)
	if !ef.Modified {
		t.Fatalf("hook did not surface JSON syntax error:\n%s", text)
	}
	if !strings.Contains(text, "[lsp]") {
		t.Fatalf("missing [lsp] header:\n%s", text)
	}
}

func TestBuiltin_E2E_YAMLErrorFoldedIntoToolResult(t *testing.T) {
	_, e, stop := liteRig(t)
	defer stop()

	dir := t.TempDir()
	ef, text := firingWrite(t, e, filepath.Join(dir, "app.yaml"), "a:\n  b: 1\n  b: 2\n")
	if !ef.Modified {
		t.Fatalf("hook did not surface YAML error:\n%s", text)
	}
	if !strings.Contains(text, "[lsp]") {
		t.Fatalf("missing [lsp] header:\n%s", text)
	}
}

func TestBuiltin_E2E_XMLErrorFoldedIntoToolResult(t *testing.T) {
	_, e, stop := liteRig(t)
	defer stop()

	dir := t.TempDir()
	ef, text := firingWrite(t, e, filepath.Join(dir, "data.xml"), `<root><a></b></root>`)
	if !ef.Modified {
		t.Fatalf("hook did not surface XML error:\n%s", text)
	}
	if !strings.Contains(text, "[lsp]") {
		t.Fatalf("missing [lsp] header:\n%s", text)
	}
}

func TestBuiltin_E2E_HTMLValidStaysSilent(t *testing.T) {
	_, e, stop := liteRig(t)
	defer stop()

	dir := t.TempDir()
	ef, text := firingWrite(t, e, filepath.Join(dir, "page.html"),
		`<!DOCTYPE html><html><body><p>hello</p></body></html>`)
	if ef.Modified || strings.Contains(text, "[lsp]") {
		t.Fatalf("valid HTML produced noise:\n%s", text)
	}
}

func TestBuiltin_E2E_CleanJSONStaysSilent(t *testing.T) {
	_, e, stop := liteRig(t)
	defer stop()

	dir := t.TempDir()
	ef, text := firingWrite(t, e, filepath.Join(dir, "config.json"), `{"a":1,"b":[2,3]}`)
	if ef.Modified || strings.Contains(text, "[lsp]") {
		t.Fatalf("clean JSON produced noise:\n%s", text)
	}
}

func TestBuiltin_E2E_ProjectWide_AcrossJSONFiles(t *testing.T) {
	_, e, stop := liteRig(t)
	defer stop()

	dir := t.TempDir()
	// Pre-write a broken JSON file.
	_, _ = firingWrite(t, e, filepath.Join(dir, "broken.json"), `{"a":1,}`)
	// Write a clean JSON file. Its own section is empty; project rollup must
	// surface broken.json — same global-view guarantee the real LSP backend
	// gives, but pure Go.
	_, text := firingWrite(t, e, filepath.Join(dir, "clean.json"), `{"a":1}`)
	if !strings.Contains(text, "[lsp] project") {
		t.Fatalf("project section missing — builtin backend lost the global view:\n%s", text)
	}
	if !strings.Contains(text, "broken.json") {
		t.Fatalf("project section did not name broken.json:\n%s", text)
	}
}
