package gitrepo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestShadowRepo_TracksAgentChanges(t *testing.T) {
	dir := t.TempDir()
	r, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	created, err := r.EnsureBaseline()
	if err != nil || !created {
		t.Fatalf("baseline: created=%v err=%v", created, err)
	}
	if ch, _ := r.Changes(); len(ch) != 0 {
		t.Fatalf("fresh baseline must be clean, got %+v", ch)
	}

	// write a new file -> added
	writeFile(t, dir, "src/app.js", "const a = 1\nconst b = 2\n")
	ch, err := r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	if len(ch) != 1 || ch[0].Path != "src/app.js" || ch[0].Status != "added" {
		t.Fatalf("expected src/app.js added, got %+v", ch)
	}
	diff, ins, del, err := r.FileDiff("src/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if ins != 2 || del != 0 {
		t.Fatalf("numstat ins/del = %d/%d, want 2/0", ins, del)
	}
	if !strings.Contains(diff, "+const a = 1") {
		t.Fatalf("unified diff missing added line:\n%s", diff)
	}

	// commit (validate) -> clean
	sha, err := r.Commit("ship", nil)
	if err != nil || sha == "" {
		t.Fatalf("commit: sha=%q err=%v", sha, err)
	}
	if ch, _ := r.Changes(); len(ch) != 0 {
		t.Fatalf("must be clean after commit, got %+v", ch)
	}

	// edit -> modified, 1 ins / 1 del vs the committed baseline
	writeFile(t, dir, "src/app.js", "const a = 1\nconst b = 99\n")
	ch, _ = r.Changes()
	if len(ch) != 1 || ch[0].Status != "modified" {
		t.Fatalf("expected modified, got %+v", ch)
	}
	if _, ins, del, _ = r.FileDiff("src/app.js"); ins != 1 || del != 1 {
		t.Fatalf("edit numstat ins/del = %d/%d, want 1/1", ins, del)
	}

	// delete -> deleted
	if err := os.Remove(filepath.Join(dir, "src", "app.js")); err != nil {
		t.Fatal(err)
	}
	ch, _ = r.Changes()
	if len(ch) != 1 || ch[0].Status != "deleted" {
		t.Fatalf("expected deleted, got %+v", ch)
	}
}

// TestShadowRepo_DoesNotTouchUserGit is THE invariant: when the workdir already
// has the user's own git repo, the shadow tracks ONLY the agent's changes and
// never touches (or mistakes for the agent's) the user's repo.
func TestShadowRepo_DoesNotTouchUserGit(t *testing.T) {
	dir := t.TempDir()

	// A real USER repo at the workdir root, with one committed file.
	userRepo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("user PlainInit: %v", err)
	}
	writeFile(t, dir, "user.txt", "hello from the user\n")
	uwt, _ := userRepo.Worktree()
	if _, err := uwt.Add("user.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := uwt.Commit("user baseline", &git.CommitOptions{
		Author: &object.Signature{Name: "user", Email: "user@x", When: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	userHeadBefore, _ := userRepo.Head()

	// Shadow repo on the same workdir.
	r, err := Open(dir)
	if err != nil {
		t.Fatalf("shadow open: %v", err)
	}
	if _, err := r.EnsureBaseline(); err != nil {
		t.Fatalf("shadow baseline: %v", err)
	}

	// Agent writes a new file.
	writeFile(t, dir, "agent.js", "agent code\n")
	ch, err := r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	// ONLY the agent file: not user.txt (in baseline), not .git, not .digitorn.
	if len(ch) != 1 || ch[0].Path != "agent.js" {
		t.Fatalf("shadow must show only agent.js, got %+v", ch)
	}

	// User repo is untouched: HEAD unchanged, still openable.
	userHeadAfter, err := userRepo.Head()
	if err != nil {
		t.Fatalf("user repo broke: %v", err)
	}
	if userHeadBefore.Hash() != userHeadAfter.Hash() {
		t.Fatalf("user HEAD moved: %s -> %s", userHeadBefore.Hash(), userHeadAfter.Hash())
	}

	// Shadow git-dir lives under .digitorn/git, separate from the user's .git.
	if _, err := os.Stat(filepath.Join(dir, ".digitorn", "git", "HEAD")); err != nil {
		t.Fatalf("shadow git-dir missing at .digitorn/git: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "HEAD")); err != nil {
		t.Fatalf("user .git was damaged: %v", err)
	}
}
