package filesystem

import (
	"strings"
	"testing"
)

func TestRead_Outline_Code(t *testing.T) {
	m, ctx := hardenModule(t)
	src := "package main\n\nimport \"fmt\"\n\nfunc Alpha() {}\n\ntype Bravo struct {\n\tX int\n}\n\nfunc (b Bravo) Charlie() {}\n\nconst Delta = 1\n"
	m.write(ctx, mustJSON(map[string]any{"path": "code.go", "content": src}))

	r, err := m.read(ctx, mustJSON(map[string]any{"path": "code.go", "outline": true}))
	if err != nil || !r.Success {
		t.Fatalf("outline read failed: %v %v", err, r.Error)
	}
	out := r.Data.(string)
	for _, want := range []string{"func Alpha", "type Bravo", "func (b Bravo) Charlie", "const Delta"} {
		if !strings.Contains(out, want) {
			t.Fatalf("outline missing %q:\n%s", want, out)
		}
	}
	// Outline must carry line numbers and must NOT include the import body line.
	if !strings.Contains(out, "func Alpha") || strings.Contains(out, "\tX int") {
		t.Fatalf("outline should list defs with line numbers, not field bodies:\n%s", out)
	}
}

func TestRead_Outline_FallsBackForPlainText(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "notes.txt", "content": "just some prose\nwith no code\nat all\n"}))
	r, err := m.read(ctx, mustJSON(map[string]any{"path": "notes.txt", "outline": true}))
	if err != nil || !r.Success {
		t.Fatalf("read failed: %v %v", err, r.Error)
	}
	// No structure → fall back to a normal numbered read (content visible).
	if !strings.Contains(r.Data.(string), "just some prose") {
		t.Fatalf("plain text with no structure should fall back to normal read:\n%s", r.Data)
	}
}

func TestRead_MultiFile(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "a.txt", "content": "AAA\n"}))
	m.write(ctx, mustJSON(map[string]any{"path": "b.txt", "content": "BBB\n"}))

	r, err := m.read(ctx, mustJSON(map[string]any{"paths": []any{"a.txt", "b.txt"}}))
	if err != nil || !r.Success {
		t.Fatalf("multi-file read failed: %v %v", err, r.Error)
	}
	out := r.Data.(string)
	if !strings.Contains(out, "===== a.txt =====") || !strings.Contains(out, "AAA") ||
		!strings.Contains(out, "===== b.txt =====") || !strings.Contains(out, "BBB") {
		t.Fatalf("multi-file read missing a labeled section:\n%s", out)
	}
}

func TestRead_MultiFile_OneMissingDoesNotAbort(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "ok.txt", "content": "HERE\n"}))

	r, err := m.read(ctx, mustJSON(map[string]any{"paths": []any{"ok.txt", "missing.txt"}}))
	if err != nil || !r.Success {
		t.Fatalf("multi-file read should succeed even with one missing: %v %v", err, r.Error)
	}
	out := r.Data.(string)
	if !strings.Contains(out, "HERE") {
		t.Fatalf("readable file must still be shown:\n%s", out)
	}
	if !strings.Contains(out, "missing.txt") || !strings.Contains(out, "error") {
		t.Fatalf("missing file should be reported inline, not abort:\n%s", out)
	}
}

// TestRead_OutlineAcceptsStringBool : a weak agent sends outline:"true" (a
// STRING). It must work, not fail with "cannot unmarshal string into bool".
func TestRead_OutlineAcceptsStringBool(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "c.go", "content": "func A(){}\nfunc B(){}\n"}))
	r, err := m.read(ctx, mustJSON(map[string]any{"path": "c.go", "outline": "true"}))
	if err != nil || !r.Success {
		t.Fatalf("outline:\"true\" (string) must work: err=%v success=%v error=%q", err, r.Success, r.Error)
	}
	if !strings.Contains(r.Data.(string), "func A") {
		t.Fatalf("outline didn't run for a string bool: %v", r.Data)
	}
}

// TestEdit_DryRunAcceptsStringBool : edit dry_run/prepend/replace_all as STRINGS
// must work (same weak-agent habit).
func TestEdit_DryRunAcceptsStringBool(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "d.txt", "content": "a\nb\n"}))
	r, err := m.edit(ctx, mustJSON(map[string]any{"path": "d.txt", "start_line": 1, "new_string": "X", "dry_run": "true"}))
	if err != nil || !r.Success {
		t.Fatalf("dry_run:\"true\" (string) must work: err=%v error=%q", err, r.Error)
	}
	if data, _ := r.Data.(map[string]any); data["dry_run"] != true {
		t.Fatalf("string dry_run not honored: %v", r.Data)
	}
	if got := readRaw(t, m, ctx, "d.txt"); got != "a\nb\n" {
		t.Fatalf("dry_run must not write: %q", got)
	}
}

func TestRead_SingleFileUnchanged(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "s.txt", "content": "l1\nl2\n"}))
	r, err := m.read(ctx, mustJSON(map[string]any{"path": "s.txt"}))
	if err != nil || !r.Success {
		t.Fatalf("single read failed: %v %v", err, r.Error)
	}
	// cat -n style: numbered, no section header.
	body := r.Data.(string)
	if strings.Contains(body, "=====") || !strings.Contains(body, "l1") || !strings.Contains(body, "l2") {
		t.Fatalf("single read should be a plain numbered slice:\n%s", body)
	}
}
