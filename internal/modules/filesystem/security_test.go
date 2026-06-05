package filesystem

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveContainment(t *testing.T) {
	ws := t.TempDir()
	m := New()
	if err := m.Init(context.Background(), map[string]any{"workspace": ws}); err != nil {
		t.Fatal(err)
	}
	// inside : ok
	if got, err := m.resolve("sub/file.txt"); err != nil {
		t.Fatalf("in-workspace path rejected: %v", err)
	} else if !strings.HasPrefix(got, ws) {
		t.Fatalf("resolved %q not under workspace %q", got, ws)
	}
	// escape via .. : rejected
	if _, err := m.resolve("../escape.txt"); err == nil {
		t.Fatal("../escape.txt should be rejected")
	}
	if _, err := m.resolve("a/../../escape.txt"); err == nil {
		t.Fatal("a/../../escape.txt should be rejected")
	}
	// absolute path : rejected
	abs := "/etc/passwd"
	if runtime.GOOS == "windows" {
		abs = `C:\Windows\system32\drivers\etc\hosts`
	}
	if _, err := m.resolve(abs); err == nil {
		t.Fatalf("absolute path %q should be rejected", abs)
	}
	// absolute path INSIDE the workspace : ACCEPTED. Agents emit absolute paths
	// once they know the workdir root ; rejecting them outright just makes the
	// model loop on the same call.
	insideAbs := filepath.Join(ws, "sub", "file.txt")
	if got, err := m.resolve(insideAbs); err != nil {
		t.Fatalf("absolute in-workspace path wrongly rejected: %v", err)
	} else if !strings.HasPrefix(got, ws) {
		t.Fatalf("resolved %q not under workspace %q", got, ws)
	}
	// sibling that merely starts with ".." is NOT an escape
	if _, err := m.resolve("..foo/bar.txt"); err != nil {
		t.Fatalf("legit '..foo' path wrongly rejected: %v", err)
	}
	_ = filepath.Separator
}
