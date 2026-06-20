package filesystem

import (
	"context"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/runtime/workdir"
)

func hardenModule(t *testing.T) (*Module, context.Context) {
	t.Helper()
	root := t.TempDir()
	m := New()
	if err := m.Init(context.Background(), map[string]any{"workspace": root}); err != nil {
		t.Fatalf("init: %v", err)
	}
	pp := workdir.NewPolicy(workdir.Options{Root: root, Home: t.TempDir()})
	return m, workdir.WithPathPolicy(context.Background(), pp)
}

// TestHardening_ReadBeyondEOF : an offset far past EOF must not panic; it returns
// cleanly (clamped), never an index-out-of-range crash.
func TestHardening_ReadBeyondEOF(t *testing.T) {
	m, ctx := hardenModule(t)
	if r, err := m.write(ctx, mustJSON(map[string]any{"path": "a.txt", "content": "l1\nl2\nl3\n"})); err != nil || !r.Success {
		t.Fatalf("write: %v %v", err, r.Error)
	}
	// offset/limit as STRINGS too (LLM habit) + far beyond EOF.
	r, err := m.read(ctx, mustJSON(map[string]any{"path": "a.txt", "offset": "999999", "limit": "50"}))
	if err != nil || !r.Success {
		t.Fatalf("read beyond EOF must succeed cleanly, got err=%v success=%v", err, r.Success)
	}
}

// TestHardening_EditNoMatch : editing with an old_string that is absent fails
// cleanly (Success=false, message), never a panic or a silent corruption.
func TestHardening_EditNoMatch(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "b.txt", "content": "hello world"}))
	r, _ := m.edit(ctx, mustJSON(map[string]any{"path": "b.txt", "old_string": "NOPE_absent", "new_string": "x"}))
	if r.Success {
		t.Fatalf("edit with absent old_string must NOT succeed")
	}
	if r.Error == "" {
		t.Fatalf("edit failure must carry an explanatory error")
	}
}

// TestHardening_EditEmptyOldString : an empty old_string is ambiguous (matches
// everywhere) and must be rejected, not silently corrupt the file.
func TestHardening_EditEmptyOldString(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "c.txt", "content": "keep me"}))
	r, _ := m.edit(ctx, mustJSON(map[string]any{"path": "c.txt", "old_string": "", "new_string": "X"}))
	if r.Success {
		t.Fatalf("edit with empty old_string must be rejected")
	}
	// file must be untouched
	rr, _ := m.read(ctx, mustJSON(map[string]any{"path": "c.txt"}))
	if !strings.Contains(rr.Data.(string), "keep me") {
		t.Fatalf("file was corrupted by an empty-old_string edit: %v", rr.Data)
	}
}

// TestHardening_EditAmbiguousShowsLines : a non-unique exact old_string must be
// refused (no silent wrong-edit) AND the error must point at WHERE the matches
// are, so the agent can target one precisely instead of blindly replace_all-ing.
func TestHardening_EditAmbiguousShowsLines(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "d.txt", "content": "x\nDUP\ny\nDUP\nz\n"}))
	r, _ := m.edit(ctx, mustJSON(map[string]any{"path": "d.txt", "old_string": "DUP", "new_string": "Q"}))
	if r.Success {
		t.Fatalf("ambiguous edit must be refused")
	}
	if !strings.Contains(r.Error, "lines") || !strings.Contains(r.Error, "2") {
		t.Fatalf("ambiguous error must report the match count + lines, got: %q", r.Error)
	}
}

// TestHardening_UnicodeRoundtrip : write then read preserves UTF-8 / emoji.
func TestHardening_UnicodeRoundtrip(t *testing.T) {
	m, ctx := hardenModule(t)
	const content = "héllo → 世界 🚀\témilie"
	m.write(ctx, mustJSON(map[string]any{"path": "u.txt", "content": content}))
	r, err := m.read(ctx, mustJSON(map[string]any{"path": "u.txt"}))
	if err != nil || !strings.Contains(r.Data.(string), "世界 🚀") {
		t.Fatalf("unicode lost in roundtrip: %q (err=%v)", r.Data, err)
	}
}

// TestHardening_EmptyFileAndNoMatchGlob : reading an empty file and globbing with
// no matches both succeed cleanly (no panic, no error).
func TestHardening_EmptyFileAndNoMatchGlob(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "empty.txt", "content": ""}))
	if r, err := m.read(ctx, mustJSON(map[string]any{"path": "empty.txt"})); err != nil || !r.Success {
		t.Fatalf("read empty file: err=%v success=%v", err, r.Success)
	}
	if r, err := m.glob(ctx, mustJSON(map[string]any{"pattern": "*.nonexistent_ext", "tree": false})); err != nil || !r.Success {
		t.Fatalf("no-match glob must succeed cleanly: err=%v", err)
	}
}
