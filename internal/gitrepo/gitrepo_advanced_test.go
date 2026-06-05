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

// changeSet collapses Changes() into a path->status map for easy assertions.
func changeSet(t *testing.T, r *Repo) map[string]string {
	t.Helper()
	ch, err := r.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	m := make(map[string]string, len(ch))
	for _, c := range ch {
		m[c.Path] = c.Status
	}
	return m
}

func fresh(t *testing.T) (*Repo, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := r.EnsureBaseline(); err != nil {
		t.Fatalf("EnsureBaseline: %v", err)
	}
	return r, dir
}

// ---- edge cases --------------------------------------------------------

func TestEdge_EmptyFile(t *testing.T) {
	r, dir := fresh(t)
	writeFile(t, dir, "empty.txt", "")
	cs := changeSet(t, r)
	if cs["empty.txt"] != "added" {
		t.Fatalf("empty file not tracked as added: %v", cs)
	}
	_, ins, del, err := r.FileDiff("empty.txt")
	if err != nil {
		t.Fatal(err)
	}
	if ins != 0 || del != 0 {
		t.Fatalf("empty file numstat = %d/%d, want 0/0", ins, del)
	}
}

func TestEdge_NoTrailingNewline(t *testing.T) {
	r, dir := fresh(t)
	writeFile(t, dir, "x.txt", "line1\nline2") // no trailing \n
	diff, ins, del, err := r.FileDiff("x.txt")
	if err != nil {
		t.Fatal(err)
	}
	if ins != 2 || del != 0 {
		t.Fatalf("numstat = %d/%d, want 2/0", ins, del)
	}
	if !strings.Contains(diff, "+line2") {
		t.Fatalf("diff missing +line2:\n%s", diff)
	}
}

func TestEdge_BinaryFileNoGarbageDiff(t *testing.T) {
	r, dir := fresh(t)
	writeFile(t, dir, "blob.bin", "PNG\x00\x01\x02\x00binarydata\x00\xff")
	cs := changeSet(t, r)
	if cs["blob.bin"] != "added" {
		t.Fatalf("binary file not tracked: %v", cs)
	}
	diff, ins, del, err := r.FileDiff("blob.bin")
	if err != nil {
		t.Fatal(err)
	}
	if ins != 0 || del != 0 || !strings.Contains(diff, "Binary") {
		t.Fatalf("binary file should report Binary, got diff=%q ins/del=%d/%d", diff, ins, del)
	}
}

func TestEdge_Unicode(t *testing.T) {
	r, dir := fresh(t)
	writeFile(t, dir, "u.txt", "café ☕\n你好世界\n🚀 ship it\n")
	diff, ins, del, err := r.FileDiff("u.txt")
	if err != nil {
		t.Fatal(err)
	}
	if ins != 3 || del != 0 {
		t.Fatalf("unicode numstat = %d/%d, want 3/0", ins, del)
	}
	if !strings.Contains(diff, "café ☕") || !strings.Contains(diff, "🚀 ship it") {
		t.Fatalf("unicode mangled:\n%s", diff)
	}
}

func TestEdge_DeepNestedPath(t *testing.T) {
	r, dir := fresh(t)
	writeFile(t, dir, "a/b/c/d/e/deep.txt", "deep\n")
	cs := changeSet(t, r)
	if cs["a/b/c/d/e/deep.txt"] != "added" {
		t.Fatalf("deep path not tracked with forward slashes: %v", cs)
	}
}

func TestEdge_MultiHunkDiff(t *testing.T) {
	r, dir := fresh(t)
	var sb strings.Builder
	for i := 1; i <= 30; i++ {
		sb.WriteString("line ")
		sb.WriteByte(byte('0' + i%10))
		sb.WriteByte('\n')
	}
	writeFile(t, dir, "big.txt", sb.String())
	if _, err := r.Commit("base", nil); err != nil {
		t.Fatal(err)
	}
	// change line 2 and line 28 -> two separate hunks
	lines := strings.Split(sb.String(), "\n")
	lines[1] = "CHANGED EARLY"
	lines[27] = "CHANGED LATE"
	writeFile(t, dir, "big.txt", strings.Join(lines, "\n"))

	diff, ins, del, err := r.FileDiff("big.txt")
	if err != nil {
		t.Fatal(err)
	}
	if h := strings.Count(diff, "\n@@ "); h != 2 {
		t.Fatalf("expected 2 hunks, got %d:\n%s", h, diff)
	}
	if ins != 2 || del != 2 {
		t.Fatalf("multi-hunk numstat = %d/%d, want 2/2", ins, del)
	}
}

func TestEdge_RenameShowsAddAndDelete(t *testing.T) {
	r, dir := fresh(t)
	writeFile(t, dir, "old.txt", "same content\n")
	if _, err := r.Commit("base", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "old.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "new.txt", "same content\n")
	cs := changeSet(t, r)
	if cs["old.txt"] != "deleted" || cs["new.txt"] != "added" {
		t.Fatalf("rename should surface delete+add, got %v", cs)
	}
}

// ---- lifecycle ---------------------------------------------------------

func TestLifecycle_ReopenIsDurable(t *testing.T) {
	dir := t.TempDir()
	r1, _ := Open(dir)
	if _, err := r1.EnsureBaseline(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "a.txt", "v1\n")
	if _, err := r1.Commit("ship a", nil); err != nil {
		t.Fatal(err)
	}

	// Simulate a daemon restart: a brand-new Repo handle on the same workdir.
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	created, err := r2.EnsureBaseline()
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("reopen must NOT recreate the baseline (HEAD already exists)")
	}
	if cs := changeSet(t, r2); len(cs) != 0 {
		t.Fatalf("reopened repo should be clean, got %v", cs)
	}
	writeFile(t, dir, "a.txt", "v2\n")
	if cs := changeSet(t, r2); cs["a.txt"] != "modified" {
		t.Fatalf("reopened repo must track new edits vs persisted HEAD, got %v", cs)
	}
}

func TestLifecycle_PartialCommit(t *testing.T) {
	r, dir := fresh(t)
	writeFile(t, dir, "keep.txt", "k\n")
	writeFile(t, dir, "ship.txt", "s\n")
	if _, err := r.Commit("only ship", []string{"ship.txt"}); err != nil {
		t.Fatal(err)
	}
	cs := changeSet(t, r)
	if _, ok := cs["ship.txt"]; ok {
		t.Fatalf("committed file must clear, got %v", cs)
	}
	if cs["keep.txt"] != "added" {
		t.Fatalf("uncommitted file must stay pending, got %v", cs)
	}
}

func TestLifecycle_BaselineIdempotent(t *testing.T) {
	dir := t.TempDir()
	r, _ := Open(dir)
	c1, err := r.EnsureBaseline()
	if err != nil || !c1 {
		t.Fatalf("first baseline: created=%v err=%v", c1, err)
	}
	c2, err := r.EnsureBaseline()
	if err != nil || c2 {
		t.Fatalf("second baseline must be a no-op: created=%v err=%v", c2, err)
	}
}

func TestLifecycle_MultipleCommitsAdvanceHead(t *testing.T) {
	r, dir := fresh(t)
	for i := 0; i < 3; i++ {
		writeFile(t, dir, "f.txt", strings.Repeat("x\n", i+1))
		if _, err := r.Commit("step", nil); err != nil {
			t.Fatal(err)
		}
		if cs := changeSet(t, r); len(cs) != 0 {
			t.Fatalf("clean expected after commit %d, got %v", i, cs)
		}
	}
}

// ---- isolation (paranoid) ---------------------------------------------

// The user has UNcommitted local changes when the agent session starts. Those
// must become part of the baseline (the starting state), never attributed to
// the agent — only the agent's later edits show.
func TestIsolation_UserUncommittedNotAttributedToAgent(t *testing.T) {
	dir := t.TempDir()
	userRepo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "file.txt", "v1 committed\n")
	uwt, _ := userRepo.Worktree()
	if _, err := uwt.Add("file.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := uwt.Commit("user v1", &git.CommitOptions{
		Author: &object.Signature{Name: "u", Email: "u@x", When: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	// User makes a local, UNCOMMITTED edit before the agent starts.
	writeFile(t, dir, "file.txt", "v2 user uncommitted\n")

	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.EnsureBaseline(); err != nil {
		t.Fatal(err)
	}
	// At baseline, NOTHING is pending: the user's uncommitted v2 is the start.
	if cs := changeSet(t, r); len(cs) != 0 {
		t.Fatalf("user's uncommitted change was mis-attributed to the agent: %v", cs)
	}

	// Agent edits -> only the agent's change shows, against the v2 baseline.
	writeFile(t, dir, "file.txt", "v3 agent edit\n")
	cs := changeSet(t, r)
	if cs["file.txt"] != "modified" {
		t.Fatalf("agent edit not tracked, got %v", cs)
	}
	diff, _, _, _ := r.FileDiff("file.txt")
	if strings.Contains(diff, "v1 committed") {
		t.Fatalf("diff must be vs the v2 baseline, not the user's v1 commit:\n%s", diff)
	}
	if !strings.Contains(diff, "v3 agent edit") {
		t.Fatalf("diff missing the agent's new content:\n%s", diff)
	}
}

func TestIsolation_UserRepoHistoryIntact(t *testing.T) {
	dir := t.TempDir()
	userRepo, _ := git.PlainInit(dir, false)
	uwt, _ := userRepo.Worktree()
	for i := 0; i < 3; i++ {
		writeFile(t, dir, "u.txt", strings.Repeat("u\n", i+1))
		uwt.Add("u.txt")
		uwt.Commit("c", &git.CommitOptions{Author: &object.Signature{Name: "u", Email: "u@x", When: time.Now()}})
	}
	headBefore, _ := userRepo.Head()
	countBefore := commitCount(t, userRepo)

	r, _ := Open(dir)
	r.EnsureBaseline()
	writeFile(t, dir, "agent.txt", "a\n")
	r.Commit("agent validates", nil) // shadow commit must not leak into user repo

	headAfter, _ := userRepo.Head()
	if headBefore.Hash() != headAfter.Hash() {
		t.Fatalf("user HEAD moved: %s -> %s", headBefore.Hash(), headAfter.Hash())
	}
	if c := commitCount(t, userRepo); c != countBefore {
		t.Fatalf("user commit count changed: %d -> %d", countBefore, c)
	}
}

func commitCount(t *testing.T, repo *git.Repository) int {
	t.Helper()
	iter, err := repo.Log(&git.LogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	_ = iter.ForEach(func(*object.Commit) error { n++; return nil })
	return n
}

// ---- robustness --------------------------------------------------------

func TestRobust_FileDiffMissingPath(t *testing.T) {
	r, _ := fresh(t)
	diff, ins, del, err := r.FileDiff("never/written.txt")
	if err != nil {
		t.Fatalf("missing path should not error: %v", err)
	}
	if diff != "" || ins != 0 || del != 0 {
		t.Fatalf("missing path should be empty, got diff=%q %d/%d", diff, ins, del)
	}
}

func TestRobust_CommitNothingIsEmptyCommit(t *testing.T) {
	r, _ := fresh(t)
	sha, err := r.Commit("", nil) // clean tree
	if err != nil {
		t.Fatalf("empty commit should be allowed: %v", err)
	}
	if sha == "" {
		t.Fatal("empty commit returned no sha")
	}
}

func TestRobust_OpenCreatesMissingWorkdir(t *testing.T) {
	base := t.TempDir()
	sub := filepath.Join(base, "not", "yet", "here")
	r, err := Open(sub)
	if err != nil {
		t.Fatalf("Open on missing workdir: %v", err)
	}
	if _, err := r.EnsureBaseline(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, sub, "ok.txt", "ok\n")
	if cs := changeSet(t, r); cs["ok.txt"] != "added" {
		t.Fatalf("workdir not usable after lazy create, got %v", cs)
	}
}
