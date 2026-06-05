package filesystem

import (
	"context"
	"strings"
	"testing"
)

// TestEdit_ReturnsClientDiff proves edit attaches the client-only diff view
// (unified diff + before/after + counts) WITHOUT leaking it into the LLM-facing
// Data. The legacy daemon's wire keys (diff/unified_diff/previous_content/
// new_content) are populated from this DiffView downstream.
func TestEdit_ReturnsClientDiff(t *testing.T) {
	m, ws := setupFS(t)
	writeFile(t, ws, "f.go", "alpha\nbeta\ngamma\n")
	r, err := m.edit(context.Background(), mustJSON(map[string]any{
		"path": "f.go", "old_string": "beta", "new_string": "BETA",
	}))
	if err != nil || !r.Success {
		t.Fatalf("edit: %v (%v)", err, r.Error)
	}
	if r.Diff == nil {
		t.Fatal("edit must return a client diff view")
	}
	if r.Diff.Additions != 1 || r.Diff.Deletions != 1 {
		t.Errorf("diff counts: +%d −%d, want +1 −1", r.Diff.Additions, r.Diff.Deletions)
	}
	if !strings.Contains(r.Diff.Unified, "-beta") || !strings.Contains(r.Diff.Unified, "+BETA") {
		t.Errorf("unified diff missing change:\n%s", r.Diff.Unified)
	}
	if r.Diff.PreviousContent != "alpha\nbeta\ngamma\n" || r.Diff.NewContent != "alpha\nBETA\ngamma\n" {
		t.Errorf("prev/new content wrong: %q → %q", r.Diff.PreviousContent, r.Diff.NewContent)
	}
	// LLM-facing Data must NOT carry the diff (it is a summary map only).
	if dmap, ok := r.Data.(map[string]any); ok {
		for k := range dmap {
			if k == "unified_diff" || k == "diff" || k == "previous_content" || k == "new_content" {
				t.Errorf("diff leaked into LLM-facing Data under %q", k)
			}
		}
	}
}

func TestEdit_FuzzyReturnsClientDiff(t *testing.T) {
	m, ws := setupFS(t)
	writeFile(t, ws, "f.go", "func main() {   \n\tx := 1\n}\n") // trailing space on line 1
	r, err := m.edit(context.Background(), mustJSON(map[string]any{
		"path": "f.go", "old_string": "func main() {\n\tx := 1", "new_string": "func main() {\n\tx := 2",
	}))
	if err != nil || !r.Success {
		t.Fatalf("fuzzy edit: %v (%v)", err, r.Error)
	}
	if r.Diff == nil || !strings.Contains(r.Diff.Unified, "+\tx := 2") {
		t.Fatalf("fuzzy edit must still produce a diff: %+v", r.Diff)
	}
}

func TestWrite_ReturnsClientDiff(t *testing.T) {
	m, ws := setupFS(t)
	ctx := context.Background()

	// Create : previous empty, everything is an addition.
	rc, err := m.write(ctx, mustJSON(map[string]any{"path": "new.txt", "content": "one\ntwo\n"}))
	if err != nil || !rc.Success {
		t.Fatalf("write create: %v (%v)", err, rc.Error)
	}
	if rc.Diff == nil || rc.Diff.PreviousContent != "" || rc.Diff.NewContent != "one\ntwo\n" {
		t.Fatalf("create diff wrong: %+v", rc.Diff)
	}
	if rc.Diff.Additions != 2 || rc.Diff.Deletions != 0 {
		t.Errorf("create counts: +%d −%d, want +2 −0", rc.Diff.Additions, rc.Diff.Deletions)
	}
	_ = ws

	// Overwrite : previous is the old content, diff shows the change.
	ro, _ := m.write(ctx, mustJSON(map[string]any{"path": "new.txt", "content": "one\nTWO\n"}))
	if ro.Diff == nil || ro.Diff.PreviousContent != "one\ntwo\n" {
		t.Fatalf("overwrite diff must carry the old content: %+v", ro.Diff)
	}
	if !strings.Contains(ro.Diff.Unified, "-two") || !strings.Contains(ro.Diff.Unified, "+TWO") {
		t.Errorf("overwrite unified diff:\n%s", ro.Diff.Unified)
	}
}

func TestWrite_IdenticalContentNoDiff(t *testing.T) {
	m, ws := setupFS(t)
	writeFile(t, ws, "f.txt", "same\n")
	r, _ := m.write(context.Background(), mustJSON(map[string]any{"path": "f.txt", "content": "same\n"}))
	if r.Diff != nil {
		t.Errorf("rewriting identical content must produce no diff, got %+v", r.Diff)
	}
}
