package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func seedBareOrigin(t *testing.T) string {
	t.Helper()
	origin := t.TempDir()
	if _, err := git.PlainInit(origin, true); err != nil {
		t.Fatal(err)
	}
	seed := t.TempDir()
	r, err := git.PlainInit(seed, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, _ := r.Worktree()
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "seed", Email: "seed@test", When: time.Now()}
	if _, err := wt.Commit("initial", &git.CommitOptions{Author: sig}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{origin}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Push(&git.PushOptions{RemoteName: "origin",
		RefSpecs: []gitconfig.RefSpec{"refs/heads/master:refs/heads/master"}}); err != nil {
		t.Fatal(err)
	}
	return origin
}

func TestNative_CloneSyncPull(t *testing.T) {
	ctx := context.Background()
	origin := seedBareOrigin(t)

	wd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wd, metaDir), 0o755); err != nil {
		t.Fatal(err)
	}
	branch, head, err := CloneRepo(ctx, wd, origin, "master", "")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if branch != "master" || head == "" {
		t.Fatalf("clone: branch=%q head=%q", branch, head)
	}
	if _, err := os.Stat(filepath.Join(wd, "README.md")); err != nil {
		t.Fatalf("cloned file missing: %v", err)
	}

	if _, _, err := CloneRepo(ctx, wd, origin, "master", ""); !errors.Is(err, ErrWorkdirNotEmpty) {
		t.Fatalf("re-clone must fail with ErrWorkdirNotEmpty, got %v", err)
	}

	if err := os.WriteFile(filepath.Join(wd, metaDir, "junk.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	unc, _, err := NativeStatus(wd)
	if err != nil || unc != 0 {
		t.Fatalf("fresh clone status: uncommitted=%d err=%v; want 0", unc, err)
	}

	if err := os.WriteFile(filepath.Join(wd, "app.go"), []byte("package app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if unc, _, _ = NativeStatus(wd); unc != 1 {
		t.Fatalf("status before sync: uncommitted=%d; want 1", unc)
	}
	head2, committed, err := NativeSync(ctx, wd, "", "add app.go", "tester", "tester@test", "master")
	if err != nil || !committed || head2 == head {
		t.Fatalf("sync: head2=%q committed=%v err=%v", head2, committed, err)
	}
	bare, err := git.PlainOpen(origin)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := bare.Reference("refs/heads/master", true)
	if err != nil || ref.Hash().String() != head2 {
		t.Fatalf("origin not advanced: ref=%v err=%v want %s", ref, err, head2)
	}
	commit, err := bare.CommitObject(ref.Hash())
	if err != nil {
		t.Fatal(err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tree.FindEntry(metaDir); err == nil {
		t.Fatalf("%s leaked into the pushed commit", metaDir)
	}
	if _, err := tree.FindEntry("app.go"); err != nil {
		t.Fatalf("app.go missing from pushed commit: %v", err)
	}

	head3, committed, err := NativeSync(ctx, wd, "", "noop", "tester", "tester@test", "master")
	if err != nil || committed || head3 != head2 {
		t.Fatalf("noop sync: head3=%q committed=%v err=%v", head3, committed, err)
	}

	wd2 := t.TempDir()
	if _, _, err := CloneRepo(ctx, wd2, origin, "master", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wd, "more.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	head4, _, err := NativeSync(ctx, wd, "", "add more.txt", "tester", "tester@test", "master")
	if err != nil {
		t.Fatal(err)
	}
	pulledHead, updated, err := NativePull(ctx, wd2, "")
	if err != nil || !updated || pulledHead != head4 {
		t.Fatalf("pull: head=%q updated=%v err=%v; want %s", pulledHead, updated, err, head4)
	}
	if _, err := os.Stat(filepath.Join(wd2, "more.txt")); err != nil {
		t.Fatalf("pulled file missing: %v", err)
	}
	if _, updated, err = NativePull(ctx, wd2, ""); err != nil || updated {
		t.Fatalf("second pull must be up-to-date: updated=%v err=%v", updated, err)
	}
}

func TestNative_InitAndFirstSync(t *testing.T) {
	ctx := context.Background()
	origin := t.TempDir()
	if _, err := git.PlainInit(origin, true); err != nil {
		t.Fatal(err)
	}
	wd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wd, metaDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := InitRepo(wd, origin, "main"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := InitRepo(wd, origin, "main"); !errors.Is(err, ErrWorkdirNotEmpty) {
		t.Fatalf("re-init must fail, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(wd, "index.html"), []byte("<html/>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	head, committed, err := NativeSync(ctx, wd, "", "first sync", "tester", "tester@test", "main")
	if err != nil || !committed || head == "" {
		t.Fatalf("first sync: head=%q committed=%v err=%v", head, committed, err)
	}
	bare, err := git.PlainOpen(origin)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := bare.Reference("refs/heads/main", true)
	if err != nil || ref.Hash().String() != head {
		t.Fatalf("origin main not pushed: ref=%v err=%v want %s", ref, err, head)
	}
	commit, _ := bare.CommitObject(ref.Hash())
	tree, _ := commit.Tree()
	if _, err := tree.FindEntry(metaDir); err == nil {
		t.Fatalf("%s leaked into pushed commit", metaDir)
	}
}
