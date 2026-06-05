package filesystem

import (
	"context"
	"strings"
	"testing"
)

// ---- multi_edit -----------------------------------------------------------

func TestMultiEdit_AppliesAllInOrder(t *testing.T) {
	m, ws := setupFS(t)
	writeFile(t, ws, "f.go", "a := 1\nb := 2\nc := 3\n")
	r, err := m.multiEdit(context.Background(), mustJSON(map[string]any{
		"path": "f.go",
		"edits": []map[string]any{
			{"old_string": "a := 1", "new_string": "a := 10"},
			{"old_string": "c := 3", "new_string": "c := 30"},
		},
	}))
	if err != nil || !r.Success {
		t.Fatalf("multi_edit: %v (%v)", err, r.Error)
	}
	if got := readFile(t, ws, "f.go"); got != "a := 10\nb := 2\nc := 30\n" {
		t.Errorf("content = %q", got)
	}
	if r.Diff == nil || r.Diff.Additions != 2 || r.Diff.Deletions != 2 {
		t.Errorf("aggregate diff wrong: %+v", r.Diff)
	}
}

func TestMultiEdit_SequentialDependsOnPrevious(t *testing.T) {
	m, ws := setupFS(t)
	writeFile(t, ws, "f.txt", "X\n")
	// Second edit only matches the result of the first.
	r, err := m.multiEdit(context.Background(), mustJSON(map[string]any{
		"path": "f.txt",
		"edits": []map[string]any{
			{"old_string": "X", "new_string": "Y"},
			{"old_string": "Y", "new_string": "Z"},
		},
	}))
	if err != nil || !r.Success {
		t.Fatalf("multi_edit: %v (%v)", err, r.Error)
	}
	if got := readFile(t, ws, "f.txt"); got != "Z\n" {
		t.Errorf("sequential edits wrong: %q", got)
	}
}

func TestMultiEdit_AtomicAllOrNothing(t *testing.T) {
	m, ws := setupFS(t)
	orig := "keep\nchange-me\n"
	writeFile(t, ws, "f.txt", orig)
	// First edit ok, second edit can't match → WHOLE op must fail, file untouched.
	r, err := m.multiEdit(context.Background(), mustJSON(map[string]any{
		"path": "f.txt",
		"edits": []map[string]any{
			{"old_string": "change-me", "new_string": "changed"},
			{"old_string": "NONEXISTENT", "new_string": "x"},
		},
	}))
	if err == nil || r.Success {
		t.Fatal("multi_edit must fail when any edit can't match")
	}
	if got := readFile(t, ws, "f.txt"); got != orig {
		t.Errorf("file MUST be untouched on partial failure, got %q", got)
	}
	if r.Data.(map[string]any)["failed_edit"] != 1 {
		t.Errorf("should report which edit failed: %v", r.Data)
	}
}

// ---- .gitignore -----------------------------------------------------------

func TestGitignore_GlobExcludes(t *testing.T) {
	m, ws := setupFS(t)
	writeFile(t, ws, ".gitignore", "*.log\nsecret/\n/build\n")
	writeFile(t, ws, "app.go", "x")
	writeFile(t, ws, "debug.log", "x")      // *.log → ignored
	writeFile(t, ws, "secret/key.txt", "x") // secret/ → ignored (dir)
	writeFile(t, ws, "build/out.bin", "x")  // /build → ignored
	writeFile(t, ws, "sub/nested.log", "x") // *.log at any depth → ignored
	writeFile(t, ws, "sub/keep.go", "x")    // kept

	r, _ := m.glob(context.Background(), mustJSON(map[string]any{"pattern": "**/*"}))
	files := r.Data.(map[string]any)["files"].([]string)
	joined := strings.Join(files, "\n")
	for _, bad := range []string{"debug.log", "secret/key.txt", "build/out.bin", "sub/nested.log"} {
		if strings.Contains(joined, bad) {
			t.Errorf(".gitignore not honoured by glob: %q present\n%s", bad, joined)
		}
	}
	for _, good := range []string{"app.go", "sub/keep.go"} {
		if !strings.Contains(joined, good) {
			t.Errorf("glob dropped a non-ignored file: %q\n%s", good, joined)
		}
	}
}

func TestGitignore_GrepExcludes(t *testing.T) {
	m, ws := setupFS(t)
	writeFile(t, ws, ".gitignore", "*.log\nvendor/\n")
	writeFile(t, ws, "real.go", "NEEDLE here\n")
	writeFile(t, ws, "trace.log", "NEEDLE here\n")     // ignored
	writeFile(t, ws, "vendor/dep.go", "NEEDLE here\n") // ignored dir

	r, err := m.grep(context.Background(), mustJSON(map[string]any{"pattern": "NEEDLE"}))
	if err != nil || !r.Success {
		t.Fatalf("grep: %v (%v)", err, r.Error)
	}
	matches := r.Data.(map[string]any)["matches"].([]grepMatch)
	if len(matches) != 1 || matches[0].File != "real.go" {
		t.Fatalf(".gitignore not honoured by grep, want only real.go: %+v", matches)
	}
}

func TestGitignore_Negation(t *testing.T) {
	m, ws := setupFS(t)
	writeFile(t, ws, ".gitignore", "*.log\n!keep.log\n")
	writeFile(t, ws, "drop.log", "x")
	writeFile(t, ws, "keep.log", "x") // re-included by negation
	r, _ := m.glob(context.Background(), mustJSON(map[string]any{"pattern": "*.log"}))
	files := r.Data.(map[string]any)["files"].([]string)
	if len(files) != 1 || files[0] != "keep.log" {
		t.Errorf("negation rule not honoured, want [keep.log], got %v", files)
	}
}

func TestParseGitignore_Syntax(t *testing.T) {
	g := parseGitignore("# comment\n\n*.tmp\n/root-only\nbuild/\n!important.tmp\n")
	if g == nil {
		t.Fatal("expected rules")
	}
	cases := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{"a.tmp", false, true},
		{"deep/b.tmp", false, true},
		{"important.tmp", false, false}, // negated
		{"root-only", false, true},
		{"sub/root-only", false, false}, // anchored to root
		{"build", true, true},
		{"x/build", true, true}, // build/ matches any depth
		{"src/main.go", false, false},
	}
	for _, c := range cases {
		if got := g.ignored(c.rel, c.isDir); got != c.want {
			t.Errorf("ignored(%q, dir=%v) = %v, want %v", c.rel, c.isDir, got, c.want)
		}
	}
}
