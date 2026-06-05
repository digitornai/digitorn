package filesystem

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/runtime/workdir"
)

// TestFilesystem_UsesCtxPolicyRoot proves WD-3: when a workdir PathPolicy rides
// on ctx, the module operates in the POLICY root (the session workdir), not its
// static config workspace, and confines glob to it.
func TestFilesystem_UsesCtxPolicyRoot(t *testing.T) {
	staticWS := t.TempDir()  // the module's static fallback workspace
	sessionWD := t.TempDir() // the per-session workdir carried on ctx
	home := t.TempDir()

	m := New()
	if err := m.Init(context.Background(), map[string]any{"workspace": staticWS}); err != nil {
		t.Fatalf("init: %v", err)
	}
	pp := workdir.NewPolicy(workdir.Options{Root: sessionWD, Home: home})
	ctx := workdir.WithPathPolicy(context.Background(), pp)

	// write lands in the SESSION workdir, not the static workspace.
	if r, err := m.write(ctx, mustJSON(map[string]any{"path": "out.txt", "content": "hi"})); err != nil || !r.Success {
		t.Fatalf("write failed: %v (%v)", err, r.Error)
	}
	if _, err := os.Stat(filepath.Join(sessionWD, "out.txt")); err != nil {
		t.Errorf("file must be in the session workdir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(staticWS, "out.txt")); err == nil {
		t.Errorf("file must NOT be in the static workspace")
	}

	// read it back (line-numbered output now contains the content).
	if r, err := m.read(ctx, mustJSON(map[string]any{"path": "out.txt"})); err != nil || !strings.Contains(r.Data.(string), "hi") {
		t.Errorf("read got %v err=%v", r.Data, err)
	}

	// glob "*.txt" finds it ; glob "../*" must NOT escape.
	if r, err := m.glob(ctx, mustJSON(map[string]any{"pattern": "*.txt"})); err != nil || len(globFiles(r)) != 1 {
		t.Errorf("glob *.txt got %v err=%v", r.Data, err)
	}
	if r, err := m.glob(ctx, mustJSON(map[string]any{"pattern": "../*"})); err != nil || len(globFiles(r)) != 0 {
		t.Errorf("glob ../* must return nothing (no escape), got %v", r.Data)
	}

	// escaping read is rejected by the policy.
	if _, err := m.read(ctx, mustJSON(map[string]any{"path": "../../../../etc/passwd"})); err == nil {
		t.Errorf("escaping read must be rejected")
	}
}

// TestFilesystem_GlobGrepDoNotFollowSymlinkOutsideWorkdir proves the symlink
// confinement fix: a symlink planted inside the workdir that points OUTSIDE
// must not let glob enumerate, nor grep read, the target. The lexical relInside
// check alone would have leaked these (the path string stays under the root).
func TestFilesystem_GlobGrepDoNotFollowSymlinkOutsideWorkdir(t *testing.T) {
	sessionWD := t.TempDir()
	outside := t.TempDir()
	home := t.TempDir()

	// A secret living entirely outside the workdir.
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOPSECRET-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}

	// link/ → outside dir, and secret.lnk → outside file. Symlink creation
	// needs privilege on Windows ; skip cleanly when unavailable.
	if err := os.Symlink(outside, filepath.Join(sessionWD, "link")); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}
	if err := os.Symlink(secret, filepath.Join(sessionWD, "secret.lnk")); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}
	// A legitimate in-workdir file so we can confirm normal results still flow.
	if err := os.WriteFile(filepath.Join(sessionWD, "real.txt"), []byte("TOPSECRET-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := New()
	if err := m.Init(context.Background(), map[string]any{"workspace": sessionWD}); err != nil {
		t.Fatalf("init: %v", err)
	}
	pp := workdir.NewPolicy(workdir.Options{Root: sessionWD, Home: home})
	ctx := workdir.WithPathPolicy(context.Background(), pp)

	// glob through the dir symlink must yield nothing (the targets resolve
	// outside the workdir).
	r, err := m.glob(ctx, mustJSON(map[string]any{"pattern": "link/*"}))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if got := globFiles(r); len(got) != 0 {
		t.Errorf("glob followed a symlink out of the workdir: %v", got)
	}

	// grep must match the REAL in-workdir file but NOT the symlinked-out file.
	gr, err := m.grep(ctx, mustJSON(map[string]any{"pattern": "TOPSECRET"}))
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	matches := gr.Data.(map[string]any)["matches"].([]grepMatch)
	for _, mm := range matches {
		if filepath.Base(mm.File) == "secret.lnk" {
			t.Errorf("grep read a file symlinked outside the workdir: %+v", mm)
		}
	}
	foundReal := false
	for _, mm := range matches {
		if filepath.Base(mm.File) == "real.txt" {
			foundReal = true
		}
	}
	if !foundReal {
		t.Errorf("grep should still match the legitimate in-workdir file; matches=%+v", matches)
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// globFiles extracts the file list from a glob tool result (Data is a map now).
func globFiles(r tool.Result) []string {
	m, ok := r.Data.(map[string]any)
	if !ok {
		return nil
	}
	files, _ := m["files"].([]string)
	return files
}
