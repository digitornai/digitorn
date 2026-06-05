package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func setupFS(t *testing.T) (*Module, string) {
	t.Helper()
	ws := t.TempDir()
	m := New()
	if err := m.Init(context.Background(), map[string]any{"workspace": ws}); err != nil {
		t.Fatalf("init: %v", err)
	}
	return m, ws
}

func writeFile(t *testing.T, ws, rel, content string) {
	t.Helper()
	p := filepath.Join(ws, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, ws, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(ws, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---- fuzzy edit -----------------------------------------------------------

func TestEdit_ExactStillWins(t *testing.T) {
	m, ws := setupFS(t)
	writeFile(t, ws, "f.txt", "alpha\nbeta\ngamma\n")
	r, err := m.edit(context.Background(), mustJSON(map[string]any{
		"path": "f.txt", "old_string": "beta", "new_string": "BETA",
	}))
	if err != nil || !r.Success {
		t.Fatalf("edit: %v (%v)", err, r.Error)
	}
	if got := r.Data.(map[string]any)["strategy"]; got != "exact" {
		t.Errorf("strategy = %v, want exact", got)
	}
	if readFile(t, ws, "f.txt") != "alpha\nBETA\ngamma\n" {
		t.Errorf("content wrong: %q", readFile(t, ws, "f.txt"))
	}
}

func TestEdit_FuzzyTrailingSpace(t *testing.T) {
	m, ws := setupFS(t)
	// The file's first line carries trailing spaces the agent's old_string omits.
	writeFile(t, ws, "f.go", "func main() {   \n\tx := 1\n}\n")
	r, err := m.edit(context.Background(), mustJSON(map[string]any{
		"path": "f.go", "old_string": "func main() {\n\tx := 1", "new_string": "func main() {\n\tx := 2",
	}))
	if err != nil || !r.Success {
		t.Fatalf("edit failed (should fuzzy-match trailing space): %v (%v)", err, r.Error)
	}
	if got := r.Data.(map[string]any)["strategy"]; got != "trailing-space" {
		t.Errorf("strategy = %v, want trailing-space", got)
	}
	if got := readFile(t, ws, "f.go"); got != "func main() {\n\tx := 2\n}\n" {
		t.Errorf("content wrong: %q", got)
	}
}

func TestEdit_FuzzyCRLF(t *testing.T) {
	m, ws := setupFS(t)
	writeFile(t, ws, "f.txt", "one\r\ntwo\r\nthree\r\n") // CRLF file
	r, err := m.edit(context.Background(), mustJSON(map[string]any{
		"path": "f.txt", "old_string": "one\ntwo", "new_string": "ONE\nTWO", // LF old_string
	}))
	if err != nil || !r.Success {
		t.Fatalf("edit failed (should fuzzy-match CRLF): %v (%v)", err, r.Error)
	}
	if got := r.Data.(map[string]any)["strategy"]; got != "line-endings" {
		t.Errorf("strategy = %v, want line-endings", got)
	}
	// The matched CRLF lines are replaced by the LF replacement (new_string
	// defines the new line endings); the untouched trailing line keeps its CRLF.
	if got := readFile(t, ws, "f.txt"); got != "ONE\nTWO\nthree\r\n" {
		t.Errorf("content wrong: %q", got)
	}
}

func TestEdit_FuzzyIndentationReindents(t *testing.T) {
	m, ws := setupFS(t)
	// File block is indented 8 spaces; the agent's old_string uses 4.
	writeFile(t, ws, "f.py", "def f():\n        a = 1\n        b = 2\n")
	r, err := m.edit(context.Background(), mustJSON(map[string]any{
		"path":       "f.py",
		"old_string": "    a = 1\n    b = 2",
		"new_string": "    a = 10\n    b = 20",
	}))
	if err != nil || !r.Success {
		t.Fatalf("edit failed (should fuzzy-match indentation): %v (%v)", err, r.Error)
	}
	if got := r.Data.(map[string]any)["strategy"]; got != "indentation" {
		t.Errorf("strategy = %v, want indentation", got)
	}
	// Replacement is re-indented to the file's 8-space level.
	if got := readFile(t, ws, "f.py"); got != "def f():\n        a = 10\n        b = 20\n" {
		t.Errorf("reindent wrong: %q", got)
	}
}

func TestEdit_TotalMissSuggestsClosest(t *testing.T) {
	m, ws := setupFS(t)
	writeFile(t, ws, "f.txt", "the quick brown fox\njumps over\nthe lazy dog\n")
	r, err := m.edit(context.Background(), mustJSON(map[string]any{
		"path": "f.txt", "old_string": "the quick brown cat", "new_string": "x",
	}))
	if err == nil || r.Success {
		t.Fatal("expected miss on absent old_string")
	}
	sugg, ok := r.Data.(map[string]any)["closest_matches"].([]suggestion)
	if !ok || len(sugg) == 0 {
		t.Fatalf("expected closest_matches suggestions, got %v", r.Data)
	}
	if sugg[0].StartLine != 1 { // "the quick brown fox" is the closest
		t.Errorf("closest match should be line 1, got %d", sugg[0].StartLine)
	}
	if readFile(t, ws, "f.txt") != "the quick brown fox\njumps over\nthe lazy dog\n" {
		t.Error("file must be untouched on a miss")
	}
}

func TestEdit_AmbiguousFuzzyNeedsReplaceAll(t *testing.T) {
	m, ws := setupFS(t)
	// Two trailing-space variants of the same line — fuzzy finds both.
	writeFile(t, ws, "f.txt", "dup  \nmid\ndup \n")
	r, err := m.edit(context.Background(), mustJSON(map[string]any{
		"path": "f.txt", "old_string": "dup", "new_string": "DUP",
	}))
	if err == nil || r.Success {
		t.Fatalf("expected ambiguous error, got success: %v", r.Data)
	}
}

// ---- grep engine ----------------------------------------------------------

func setupTree(t *testing.T) (*Module, string) {
	t.Helper()
	m, ws := setupFS(t)
	writeFile(t, ws, "a.go", "package a\nfunc Foo() {}\n// TODO: x\n")
	writeFile(t, ws, "b.go", "package b\nfunc Bar() {}\nfunc Foo2() {}\n")
	writeFile(t, ws, "sub/c.txt", "hello Foo world\nplain line\n")
	writeFile(t, ws, "node_modules/dep.go", "func Foo() {} // should be skipped\n")
	// a binary file (NUL byte) must be skipped
	writeFile(t, ws, "bin.dat", "Foo\x00\x01binary")
	return m, ws
}

func TestGrep_ContentModeWithSkipDirsAndBinary(t *testing.T) {
	m, ws := setupTree(t)
	_ = ws
	r, err := m.grep(context.Background(), mustJSON(map[string]any{"pattern": "Foo"}))
	if err != nil || !r.Success {
		t.Fatalf("grep: %v (%v)", err, r.Error)
	}
	matches := r.Data.(map[string]any)["matches"].([]grepMatch)
	for _, mm := range matches {
		if filepath.Base(mm.File) == "dep.go" {
			t.Errorf("grep descended into node_modules: %+v", mm)
		}
		if filepath.Base(mm.File) == "bin.dat" {
			t.Errorf("grep scanned a binary file: %+v", mm)
		}
	}
	// Foo appears in a.go:2, b.go:3 (Foo2), sub/c.txt:1 → 3 matches, sorted.
	if len(matches) != 3 {
		t.Fatalf("want 3 matches, got %d: %+v", len(matches), matches)
	}
	if matches[0].File != "a.go" || matches[1].File != "b.go" || matches[2].File != "sub/c.txt" {
		t.Errorf("matches not sorted by path: %+v", matches)
	}
}

func TestGrep_FilesAndCountModes(t *testing.T) {
	m, _ := setupTree(t)
	rf, _ := m.grep(context.Background(), mustJSON(map[string]any{"pattern": "Foo", "output_mode": "files_with_matches"}))
	files := rf.Data.(map[string]any)["files"].([]string)
	if len(files) != 3 {
		t.Errorf("files mode: want 3, got %v", files)
	}
	rc, _ := m.grep(context.Background(), mustJSON(map[string]any{"pattern": "Foo", "output_mode": "count"}))
	if c := rc.Data.(map[string]any)["count"].(int); c != 3 {
		t.Errorf("count mode: want 3, got %d", c)
	}
}

func TestGrep_Context(t *testing.T) {
	m, ws := setupFS(t)
	writeFile(t, ws, "f.txt", "l1\nl2\nMATCH\nl4\nl5\n")
	r, _ := m.grep(context.Background(), mustJSON(map[string]any{"pattern": "MATCH", "context": 1}))
	matches := r.Data.(map[string]any)["matches"].([]grepMatch)
	if len(matches) != 1 {
		t.Fatalf("want 1 match, got %d", len(matches))
	}
	if len(matches[0].Before) != 1 || matches[0].Before[0] != "l2" {
		t.Errorf("before context wrong: %v", matches[0].Before)
	}
	if len(matches[0].After) != 1 || matches[0].After[0] != "l4" {
		t.Errorf("after context wrong: %v", matches[0].After)
	}
}

func TestGrep_RegexAndInclude(t *testing.T) {
	m, ws := setupFS(t)
	writeFile(t, ws, "a.go", "func Alpha() {}\n")
	writeFile(t, ws, "a.txt", "func Alpha() {}\n")
	r, _ := m.grep(context.Background(), mustJSON(map[string]any{
		"pattern": `func \w+\(`, "include": "*.go",
	}))
	matches := r.Data.(map[string]any)["matches"].([]grepMatch)
	if len(matches) != 1 || matches[0].File != "a.go" {
		t.Errorf("include filter / regex wrong: %+v", matches)
	}
}

func TestGrep_Multiline(t *testing.T) {
	m, ws := setupFS(t)
	writeFile(t, ws, "f.txt", "start\nA\nB\nend\n")
	r, _ := m.grep(context.Background(), mustJSON(map[string]any{
		"pattern": "A.*B", "multiline": true,
	}))
	matches := r.Data.(map[string]any)["matches"].([]grepMatch)
	if len(matches) != 1 || matches[0].LineNum != 2 {
		t.Errorf("multiline match wrong: %+v", matches)
	}
}

func TestGrep_Truncation(t *testing.T) {
	m, ws := setupFS(t)
	for i := 0; i < 50; i++ {
		writeFile(t, ws, filepath.Join("d", filepathBase(i)), "needle\n")
	}
	r, _ := m.grep(context.Background(), mustJSON(map[string]any{"pattern": "needle", "max_results": 10}))
	d := r.Data.(map[string]any)
	matches := d["matches"].([]grepMatch)
	if len(matches) != 10 {
		t.Errorf("want capped at 10, got %d", len(matches))
	}
	if d["truncated"] != true {
		t.Errorf("truncated flag not set")
	}
}

func filepathBase(i int) string {
	return "f" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + ".txt"
}
