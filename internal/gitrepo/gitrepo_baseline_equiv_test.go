package gitrepo

import (
	"os"
	"testing"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// baselineTreeOldWay reproduces the PRE-change baseline (go-git
// AddWithOptions{All:true} + commit) on a workdir and returns its root tree hash.
func baselineTreeOldWay(t *testing.T, workdir string) string {
	t.Helper()
	gd := gitDirOf(workdir)
	if err := os.MkdirAll(gd, 0o755); err != nil {
		t.Fatal(err)
	}
	storer := noGitlinkStorer{filesystem.NewStorage(osfs.New(gd), cache.NewObjectLRUDefault())}
	repo, err := git.Init(storer, osfs.New(workdir))
	if err != nil {
		t.Fatal(err)
	}
	ensureExclude(gd)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	// Replicate Open's worktree excludes so go-git's Add skips the shadow metadata
	// and any user .git, exactly as production does.
	wt.Excludes = append(wt.Excludes,
		gitignore.ParsePattern(metaDir+"/", nil),
		gitignore.ParsePattern(".git/", nil),
	)
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	h, err := wt.Commit("baseline", &git.CommitOptions{AllowEmptyCommits: true, Author: signature()})
	if err != nil {
		t.Fatal(err)
	}
	c, err := repo.CommitObject(h)
	if err != nil {
		t.Fatal(err)
	}
	return c.TreeHash.String()
}

func headTreeHash(t *testing.T, r *Repo) string {
	t.Helper()
	ref, err := r.repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatal(err)
	}
	return c.TreeHash.String()
}

// TestBaseline_OnePassMatchesGoGitAdd proves the one-pass baseline builds the EXACT
// same tree as go-git's AddWithOptions{All:true}: same files, same content, same
// root tree hash. If they ever diverge, every later diff/status against the
// baseline would drift — this is the guard against that.
func TestBaseline_OnePassMatchesGoGitAdd(t *testing.T) {
	files := map[string]string{
		"a.txt":              "alpha\nbeta\n",
		"b.go":               "package main\n\nfunc main() {}\n",
		"nested/deep/c.json": "{\n  \"k\": 1\n}\n",
		"empty.txt":          "",
		"sub/d.md":           "# title\n\ntext\n",
	}

	dirNew := t.TempDir()
	for rel, content := range files {
		writeFile(t, dirNew, rel, content)
	}
	r, err := Open(dirNew)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.EnsureBaseline(); err != nil {
		t.Fatal(err)
	}
	newTree := headTreeHash(t, r)

	dirOld := t.TempDir()
	for rel, content := range files {
		writeFile(t, dirOld, rel, content)
	}
	oldTree := baselineTreeOldWay(t, dirOld)

	if newTree != oldTree {
		t.Fatalf("baseline tree mismatch: one-pass=%s go-git-Add=%s", newTree, oldTree)
	}

	ch, err := r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	if len(ch) != 0 {
		t.Fatalf("fresh one-pass baseline must be clean, got %+v", ch)
	}
}
