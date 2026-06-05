package lsp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Real pyright integration. Skips if pyright-langserver is absent.
func TestPyrightDiagnostics(t *testing.T) {
	if _, err := exec.LookPath("pyright-langserver"); err != nil {
		t.Skip("pyright-langserver not installed")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "buggy.py")
	content := "def add(a, b):\n    return a + b\n\nresult = add(2, \"3\")\nprint(res)\n"
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr := newManager(builtinSpecs(), 25*time.Second) // generous settle for Node cold start
	defer mgr.stopAll(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start := time.Now()
	diags, err := mgr.notifyChange(ctx, file, content)
	t.Logf("pyright notifyChange took %s, returned %d diagnostic(s)", time.Since(start).Round(time.Millisecond), len(diags))
	if err != nil {
		t.Fatalf("notifyChange: %v", err)
	}
	for _, d := range diags {
		t.Logf("  %s:%d:%d [%s] %s (%s)", filepath.Base(d.File), d.Line, d.Column, d.Severity, d.Message, d.Source)
	}
	if len(diags) == 0 {
		t.Fatal("pyright returned no diagnostics for a buggy file (cold-start slower than settle?)")
	}
}
