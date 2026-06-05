package filesystem

import (
	"context"
	"os"
	"strings"
	"testing"
)

// surgicalSetup seeds f.txt with content and returns the module + ctx.
func surgicalSetup(t *testing.T, content string) (*Module, context.Context) {
	t.Helper()
	m, ctx := hardenModule(t)
	if r, err := m.write(ctx, mustJSON(map[string]any{"path": "f.txt", "content": content})); err != nil || !r.Success {
		t.Fatalf("seed write: %v %v", err, r.Error)
	}
	return m, ctx
}

// readRaw returns the file's exact bytes (bypassing the numbered read).
func readRaw(t *testing.T, m *Module, ctx context.Context, rel string) string {
	t.Helper()
	abs, err := m.resolveCtx(ctx, rel)
	if err != nil {
		t.Fatalf("resolve %q: %v", rel, err)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("readfile %q: %v", rel, err)
	}
	return string(b)
}

func TestSurgical_LineRange_Replace(t *testing.T) {
	m, ctx := surgicalSetup(t, "alpha\nbravo\ncharlie\ndelta\n")
	r, err := m.edit(ctx, mustJSON(map[string]any{
		"path": "f.txt", "start_line": 2, "end_line": 3, "new_string": "BRAVO\nCHARLIE",
	}))
	if err != nil || !r.Success {
		t.Fatalf("line-range replace failed: %v %v", err, r.Error)
	}
	if got := readRaw(t, m, ctx, "f.txt"); got != "alpha\nBRAVO\nCHARLIE\ndelta\n" {
		t.Fatalf("got %q", got)
	}
}

func TestSurgical_LineRange_Delete(t *testing.T) {
	m, ctx := surgicalSetup(t, "a\nb\nc\n")
	r, err := m.edit(ctx, mustJSON(map[string]any{"path": "f.txt", "start_line": 2, "new_string": ""}))
	if err != nil || !r.Success {
		t.Fatalf("delete failed: %v %v", err, r.Error)
	}
	if got := readRaw(t, m, ctx, "f.txt"); got != "a\nc\n" {
		t.Fatalf("got %q", got)
	}
}

func TestSurgical_LineRange_OutOfRange(t *testing.T) {
	m, ctx := surgicalSetup(t, "a\nb\n")
	r, _ := m.edit(ctx, mustJSON(map[string]any{"path": "f.txt", "start_line": 99, "new_string": "x"}))
	if r.Success {
		t.Fatalf("out-of-range start_line must fail")
	}
	if !strings.Contains(r.Error, "past end of file") {
		t.Fatalf("error should explain range: %q", r.Error)
	}
}

func TestSurgical_LineRange_ExpectMismatch(t *testing.T) {
	m, ctx := surgicalSetup(t, "a\nb\nc\n")
	r, _ := m.edit(ctx, mustJSON(map[string]any{
		"path": "f.txt", "start_line": 2, "new_string": "X", "expect": "NOT_THERE",
	}))
	if r.Success {
		t.Fatalf("expect mismatch must refuse the edit")
	}
	if got := readRaw(t, m, ctx, "f.txt"); got != "a\nb\nc\n" {
		t.Fatalf("file must be untouched on expect mismatch, got %q", got)
	}
}

func TestSurgical_InsertAfter(t *testing.T) {
	m, ctx := surgicalSetup(t, "import x\ncode\n")
	r, err := m.edit(ctx, mustJSON(map[string]any{
		"path": "f.txt", "insert_after": "import x", "new_string": "import y",
	}))
	if err != nil || !r.Success {
		t.Fatalf("insert_after failed: %v %v", err, r.Error)
	}
	if got := readRaw(t, m, ctx, "f.txt"); got != "import x\nimport y\ncode\n" {
		t.Fatalf("got %q", got)
	}
}

func TestSurgical_InsertBefore(t *testing.T) {
	m, ctx := surgicalSetup(t, "body\n")
	r, err := m.edit(ctx, mustJSON(map[string]any{
		"path": "f.txt", "insert_before": "body", "new_string": "header",
	}))
	if err != nil || !r.Success {
		t.Fatalf("insert_before failed: %v %v", err, r.Error)
	}
	if got := readRaw(t, m, ctx, "f.txt"); got != "header\nbody\n" {
		t.Fatalf("got %q", got)
	}
}

func TestSurgical_InsertAnchorAmbiguous(t *testing.T) {
	m, ctx := surgicalSetup(t, "dup\nx\ndup\n")
	r, _ := m.edit(ctx, mustJSON(map[string]any{
		"path": "f.txt", "insert_after": "dup", "new_string": "z",
	}))
	if r.Success {
		t.Fatalf("ambiguous anchor must be refused")
	}
	if !strings.Contains(r.Error, "matches 2 lines") {
		t.Fatalf("error should report the ambiguity + lines: %q", r.Error)
	}
}

func TestSurgical_Prepend(t *testing.T) {
	m, ctx := surgicalSetup(t, "@tailwind base;\n")
	r, err := m.edit(ctx, mustJSON(map[string]any{"path": "f.txt", "prepend": true, "new_string": "/* top */"}))
	if err != nil || !r.Success {
		t.Fatalf("prepend failed: %v %v", err, r.Error)
	}
	if got := readRaw(t, m, ctx, "f.txt"); got != "/* top */\n@tailwind base;\n" {
		t.Fatalf("got %q", got)
	}
}

func TestSurgical_Append(t *testing.T) {
	m, ctx := surgicalSetup(t, "line1\n")
	r, err := m.edit(ctx, mustJSON(map[string]any{"path": "f.txt", "append": true, "new_string": "line2"}))
	if err != nil || !r.Success {
		t.Fatalf("append failed: %v %v", err, r.Error)
	}
	if got := readRaw(t, m, ctx, "f.txt"); got != "line1\nline2\n" {
		t.Fatalf("got %q", got)
	}
}

func TestSurgical_OccurrenceNth(t *testing.T) {
	m, ctx := surgicalSetup(t, "v=1; v=1; v=1;\n")
	r, err := m.edit(ctx, mustJSON(map[string]any{
		"path": "f.txt", "old_string": "v=1", "new_string": "v=2", "occurrence": 2,
	}))
	if err != nil || !r.Success {
		t.Fatalf("occurrence failed: %v %v", err, r.Error)
	}
	if got := readRaw(t, m, ctx, "f.txt"); got != "v=1; v=2; v=1;\n" {
		t.Fatalf("got %q (only the 2nd should change)", got)
	}
}

func TestSurgical_OccurrenceOutOfRange(t *testing.T) {
	m, ctx := surgicalSetup(t, "a a\n")
	r, _ := m.edit(ctx, mustJSON(map[string]any{
		"path": "f.txt", "old_string": "a", "new_string": "b", "occurrence": 9,
	}))
	if r.Success {
		t.Fatalf("occurrence past the match count must fail")
	}
	if !strings.Contains(r.Error, "out of range") {
		t.Fatalf("error should say out of range: %q", r.Error)
	}
}

func TestSurgical_DryRunDoesNotWrite(t *testing.T) {
	m, ctx := surgicalSetup(t, "a\nb\nc\n")
	r, err := m.edit(ctx, mustJSON(map[string]any{
		"path": "f.txt", "start_line": 1, "new_string": "Z", "dry_run": true,
	}))
	if err != nil || !r.Success {
		t.Fatalf("dry_run should succeed: %v %v", err, r.Error)
	}
	data, _ := r.Data.(map[string]any)
	if data["dry_run"] != true {
		t.Fatalf("result must be flagged dry_run: %v", data)
	}
	if data["diff"] == nil || !strings.Contains(data["diff"].(string), "Z") {
		t.Fatalf("dry_run must return a diff preview containing the change: %v", data["diff"])
	}
	if got := readRaw(t, m, ctx, "f.txt"); got != "a\nb\nc\n" {
		t.Fatalf("dry_run must NOT write, file changed to %q", got)
	}
}

// TestSurgical_MultipleLocatorsResolveByPrecedence : a weak agent that supplies
// BOTH old_string and a (conflicting) start_line must not fail — the tool
// resolves by precedence and uses old_string (the self-validating one). Here
// old_string targets line 3 while start_line points at line 1 ; old_string must
// win, proving precedence rather than positional editing.
func TestSurgical_MultipleLocatorsResolveByPrecedence(t *testing.T) {
	m, ctx := surgicalSetup(t, "a\nb\nc\n")
	r, err := m.edit(ctx, mustJSON(map[string]any{
		"path": "f.txt", "start_line": 1, "old_string": "c", "new_string": "x",
	}))
	if err != nil || !r.Success {
		t.Fatalf("multiple locators must resolve, not reject: err=%v res=%+v", err, r)
	}
	if got := readRaw(t, m, ctx, "f.txt"); got != "a\nb\nx\n" {
		t.Fatalf("old_string must win over start_line (expected line 3 edited): got %q", got)
	}
}

func TestSurgical_NoTrailingNewlinePreserved(t *testing.T) {
	m, ctx := surgicalSetup(t, "a\nb") // NO trailing newline
	r, err := m.edit(ctx, mustJSON(map[string]any{"path": "f.txt", "start_line": 1, "new_string": "A"}))
	if err != nil || !r.Success {
		t.Fatalf("edit failed: %v %v", err, r.Error)
	}
	if got := readRaw(t, m, ctx, "f.txt"); got != "A\nb" {
		t.Fatalf("trailing-newline convention not preserved: got %q want %q", got, "A\nb")
	}
}

func TestSurgical_MultiEditSequentialAnchors(t *testing.T) {
	m, ctx := surgicalSetup(t, "header\nbody\n")
	r, err := m.multiEdit(ctx, mustJSON(map[string]any{
		"path": "f.txt",
		"edits": []any{
			map[string]any{"insert_after": "header", "new_string": "after-header"},
			map[string]any{"old_string": "body", "new_string": "BODY"},
		},
	}))
	if err != nil || !r.Success {
		t.Fatalf("multi_edit failed: %v %v", err, r.Error)
	}
	if got := readRaw(t, m, ctx, "f.txt"); got != "header\nafter-header\nBODY\n" {
		t.Fatalf("got %q", got)
	}
}
