package gitrepo

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

func buildTree(t *testing.T, dir string, srcFiles, nodeModFiles int) {
	t.Helper()
	writeFile(t, dir, ".gitignore", "node_modules/\n")
	for i := 0; i < srcFiles; i++ {
		writeFile(t, dir, fmt.Sprintf("src/pkg%02d/file%04d.go", i%50, i), fmt.Sprintf("package pkg%02d\n\nvar X%04d = %d\n", i%50, i, i))
	}
	for i := 0; i < nodeModFiles; i++ {
		writeFile(t, dir, fmt.Sprintf("node_modules/dep%03d/mod%05d.js", i%200, i), fmt.Sprintf("module.exports = %d\n", i))
	}
}

// oldRepo reproduces the PRE-change setup: a raw go-git repo over an UNPRUNED
// worktree FS, so AddWithOptions{All} and wt.Status() behave as before the fix.
func oldRepo(t *testing.T, workdir string) *git.Worktree {
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
	wt.Excludes = append(wt.Excludes,
		gitignore.ParsePattern(metaDir+"/", nil),
		gitignore.ParsePattern(".git/", nil),
	)
	return wt
}

// TestPerf_BaselineAndStatus compares OLD (go-git AddWithOptions{All} + unpruned
// wt.Status) vs NEW (one-pass baseline + pruning FS) on a realistic tree: a few
// thousand source files plus a large gitignored node_modules. Gated on PERF=1.
//
//	PERF=1 go test ./internal/gitrepo -run TestPerf_BaselineAndStatus -v -timeout 1200s
func TestPerf_BaselineAndStatus(t *testing.T) {
	if os.Getenv("PERF") == "" {
		t.Skip("set PERF=1 to run the benchmark")
	}
	const srcFiles = 12000
	const nodeModFiles = 3000

	dirNew := t.TempDir()
	dirOld := t.TempDir()
	buildTree(t, dirNew, srcFiles, nodeModFiles)
	buildTree(t, dirOld, srcFiles, nodeModFiles)
	t.Logf("tree: %d source + %d node_modules files (each)", srcFiles, nodeModFiles)

	// OLD path FIRST (so any cold-cache penalty lands on OLD, not NEW — a fair-or-
	// pessimistic read for the new code).
	wtOld := oldRepo(t, dirOld)
	tb := time.Now()
	if err := wtOld.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := wtOld.Commit("baseline", &git.CommitOptions{AllowEmptyCommits: true, Author: signature()}); err != nil {
		t.Fatal(err)
	}
	oldBaseline := time.Since(tb)
	ts := time.Now()
	stOld, err := wtOld.Status()
	if err != nil {
		t.Fatal(err)
	}
	oldStatus := time.Since(ts)

	// NEW path SECOND.
	r, err := Open(dirNew)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	if _, err := r.EnsureBaseline(); err != nil {
		t.Fatal(err)
	}
	newBaseline := time.Since(t0)
	t1 := time.Now()
	chNew, err := r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	newStatus := time.Since(t1)

	t.Logf("BASELINE   old(go-git Add): %-13v  new(one-pass): %v", oldBaseline, newBaseline)
	t.Logf("STATUS     old(unpruned):   %-13v  new(pruned):   %v", oldStatus, newStatus)
	t.Logf("clean change-set: new=%d  old(status entries)=%d", len(chNew), len(stOld))
}
