package gitrepo

import (
	"os"
	"path/filepath"
	"testing"
)

// An empty workdir must NOT get a .digitorn shadow — otherwise a scaffolder that
// requires an empty directory (npm create, git clone …) fails at session start.
func TestOpenIfNeeded_EmptyWorkdirDoesNotCreate(t *testing.T) {
	wd := t.TempDir()
	r, err := OpenIfNeeded(wd)
	if err != nil {
		t.Fatalf("OpenIfNeeded: %v", err)
	}
	if r != nil {
		t.Fatal("expected nil repo for an empty workdir")
	}
	if _, err := os.Stat(filepath.Join(wd, metaDir)); !os.IsNotExist(err) {
		t.Fatalf("%s was created on an empty workdir", metaDir)
	}
	if entries, _ := os.ReadDir(wd); len(entries) != 0 {
		t.Fatalf("workdir is no longer empty: %v", entries)
	}
}

// Once the workdir has real content (the agent scaffolded), the next read
// creates the shadow so the files get tracked.
func TestOpenIfNeeded_WithContentCreates(t *testing.T) {
	wd := t.TempDir()
	if err := os.WriteFile(filepath.Join(wd, "index.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := OpenIfNeeded(wd)
	if err != nil {
		t.Fatalf("OpenIfNeeded: %v", err)
	}
	if r == nil {
		t.Fatal("expected a repo for a non-empty workdir")
	}
	if _, err := os.Stat(gitDirOf(wd)); err != nil {
		t.Fatalf("%s/git not created for a non-empty workdir: %v", metaDir, err)
	}
}

// A pre-existing shadow (from a prior session) opens even if the only remaining
// entry is .digitorn itself — never regressing an already-tracked workspace.
func TestOpenIfNeeded_ExistingShadowOpens(t *testing.T) {
	wd := t.TempDir()
	if _, err := Open(wd); err != nil {
		t.Fatalf("seed Open: %v", err)
	}
	r, err := OpenIfNeeded(wd)
	if err != nil {
		t.Fatalf("OpenIfNeeded: %v", err)
	}
	if r == nil {
		t.Fatal("expected an existing shadow to open")
	}
}

// A user's pre-existing .git must not count as "content" — an empty workdir that
// only carries a .git still defers shadow creation.
func TestWorkdirHasContent_IgnoresMeta(t *testing.T) {
	wd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wd, metaDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if workdirHasContent(wd) {
		t.Fatal(".git/.digitorn should not count as trackable content")
	}
	if err := os.WriteFile(filepath.Join(wd, "app.tsx"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !workdirHasContent(wd) {
		t.Fatal("a real file should count as content")
	}
}
