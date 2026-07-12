package workspace

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/gitrepo"
)

func mj(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func filesOf(t *testing.T, res any) []gitrepo.Change {
	t.Helper()
	data, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("result data not a map: %T", res)
	}
	files, ok := data["files"].([]gitrepo.Change)
	if !ok {
		t.Fatalf("files not []gitrepo.Change: %T", data["files"])
	}
	return files
}

func TestModule_BaselineChangesDiffCommit(t *testing.T) {
	dir := t.TempDir()
	m := New()
	ctx := context.Background()

	// An EMPTY workdir must NOT get a baseline (no .digitorn) — otherwise a
	// scaffolder that needs an empty dir (npm create, git clone…) fails.
	res, err := m.baseline(ctx, mj(map[string]any{"workdir": dir}))
	if err != nil || !res.Success {
		t.Fatalf("baseline(empty): err=%v res=%+v", err, res)
	}
	if created, _ := res.Data.(map[string]any)["created"].(bool); created {
		t.Fatalf("baseline must NOT create on an empty workdir: %+v", res.Data)
	}
	if _, serr := os.Stat(filepath.Join(dir, ".digitorn")); !os.IsNotExist(serr) {
		t.Fatal(".digitorn was created on an empty workdir")
	}

	// The agent scaffolds the project — these files are the STARTING state, so
	// they must NOT surface as pending "added" changes.
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("const a = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = m.baseline(ctx, mj(map[string]any{"workdir": dir}))
	if err != nil || !res.Success {
		t.Fatalf("baseline(populated): err=%v res=%+v", err, res)
	}
	if created, _ := res.Data.(map[string]any)["created"].(bool); !created {
		t.Fatalf("baseline should create once the workdir has content: %+v", res.Data)
	}
	res, _ = m.changes(ctx, mj(map[string]any{"workdir": dir}))
	if files := filesOf(t, res.Data); len(files) != 0 {
		t.Fatalf("scaffolded files must be the baseline, not changes: %+v", files)
	}

	// The agent then EDITS a scaffolded file — THAT is a real change.
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("const a = 2\nconst b = 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = m.changes(ctx, mj(map[string]any{"workdir": dir}))
	if err != nil || !res.Success {
		t.Fatalf("changes: err=%v res=%+v", err, res)
	}
	files := filesOf(t, res.Data)
	if len(files) != 1 || files[0].Path != "app.js" || files[0].Status != "modified" {
		t.Fatalf("the edit should show as a modified change: %+v", files)
	}

	res, err = m.diff(ctx, mj(map[string]any{"workdir": dir, "path": "app.js"}))
	if err != nil || !res.Success {
		t.Fatalf("diff: err=%v res=%+v", err, res)
	}
	d := res.Data.(map[string]any)
	if !strings.Contains(d["unified"].(string), "+const a = 2") {
		t.Fatalf("diff unified missing the edit:\n%s", d["unified"])
	}

	// Approve COMMITS the change as one revision (approval = a committed revision).
	res, err = m.approve(ctx, mj(map[string]any{"workdir": dir, "paths": []string{"app.js"}, "message": "ship"}))
	if err != nil || !res.Success {
		t.Fatalf("approve: err=%v res=%+v", err, res)
	}
	if sha, _ := res.Data.(map[string]any)["sha"].(string); sha == "" {
		t.Fatal("approve returned no sha")
	}

	// Once approved (committed) the file stops showing as pending.
	res, _ = m.changes(ctx, mj(map[string]any{"workdir": dir}))
	if files := filesOf(t, res.Data); len(files) != 0 {
		t.Fatalf("should be clean after approve, got %+v", files)
	}

	// The approval is a revision in the file's history, labelled by its message.
	res, err = m.history(ctx, mj(map[string]any{"workdir": dir, "path": "app.js"}))
	if err != nil || !res.Success {
		t.Fatalf("history: err=%v res=%+v", err, res)
	}
	revs, _ := res.Data.(map[string]any)["revisions"].([]gitrepo.Revision)
	if len(revs) == 0 || revs[len(revs)-1].Message != "ship" {
		t.Fatalf("history must include the approval revision labelled 'ship': %+v", res.Data)
	}
}

func TestModule_RepoCacheReused(t *testing.T) {
	dir := t.TempDir()
	m := New()
	r1, err := m.repo(dir)
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := m.repo(dir)
	if r1 != r2 {
		t.Fatal("repo cache must return the same instance for the same workdir")
	}
}

func TestModule_ValidatesParams(t *testing.T) {
	m := New()
	ctx := context.Background()
	if res, _ := m.changes(ctx, mj(map[string]any{})); res.Success || res.Error == "" {
		t.Fatal("changes without workdir must error")
	}
	if res, _ := m.diff(ctx, mj(map[string]any{"workdir": "x"})); res.Success || res.Error == "" {
		t.Fatal("diff without path must error")
	}
	if res, _ := m.baseline(ctx, mj(map[string]any{})); res.Success || res.Error == "" {
		t.Fatal("baseline without workdir must error")
	}
}
