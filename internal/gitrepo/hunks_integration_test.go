package gitrepo

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readWT(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// numbered builds a 20-line file "a01".."a20\n", applying the given per-line
// edits. Lines 2 and 19 are far enough apart that the diff keeps them as TWO
// separate hunks (their default context windows don't overlap) — the case the
// hunk-level approve/reject is actually for.
func numbered(edits map[int]string) string {
	var b strings.Builder
	for i := 1; i <= 20; i++ {
		line := fmt.Sprintf("a%02d", i)
		if e, ok := edits[i]; ok {
			line = e
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func twoHunkRepo(t *testing.T) (*Repo, string, []diffHunk) {
	t.Helper()
	dir := t.TempDir()
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "f.txt", numbered(nil))
	commitAll(t, r, "baseline")
	writeFile(t, dir, "f.txt", numbered(map[int]string{2: "X02", 19: "X19"}))
	unified, _, _, err := r.FileDiff("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	hunks := parseUnifiedHunks(unified)
	if len(hunks) != 2 {
		t.Fatalf("expected 2 hunks, got %d:\n%s", len(hunks), unified)
	}
	// hunk[0] = the line-2 edit, hunk[1] = the line-19 edit (ascending).
	return r, dir, hunks
}

func TestApproveHunks_CommitsOnlySelected(t *testing.T) {
	r, dir, hunks := twoHunkRepo(t)
	current := numbered(map[int]string{2: "X02", 19: "X19"})
	if err := r.ApproveHunks("f.txt", []string{hunks[0].Hash}, ""); err != nil {
		t.Fatal(err)
	}
	if got := readWT(t, dir, "f.txt"); got != current {
		t.Fatalf("worktree must keep full content, got:\n%s", got)
	}
	// baseline advanced by hunk0 (X02) but NOT hunk1 (still a19)
	base, _, err := r.headBlobExists("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(base, "X02") || !strings.Contains(base, "a19") || strings.Contains(base, "X19") {
		t.Fatalf("baseline should be line-2-approved only:\n%s", base)
	}
	// remaining diff = only the X19 hunk
	unified2, _, _, err := r.FileDiff("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	rem := parseUnifiedHunks(unified2)
	if len(rem) != 1 || !strings.Contains(strings.Join(rem[0].Body, "\n"), "X19") {
		t.Fatalf("expected one remaining hunk (X19), got %d:\n%s", len(rem), unified2)
	}
}

func TestRejectHunks_RevertsOnlySelected(t *testing.T) {
	r, dir, hunks := twoHunkRepo(t)
	if err := r.RejectHunks("f.txt", []string{hunks[0].Hash}); err != nil {
		t.Fatal(err)
	}
	// line 2 reverted to baseline (a02), line 19 edit kept (X19)
	want := numbered(map[int]string{19: "X19"})
	if got := readWT(t, dir, "f.txt"); got != want {
		t.Fatalf("reject got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRejectHunks_AllRevertsFile(t *testing.T) {
	r, dir, hunks := twoHunkRepo(t)
	if err := r.RejectHunks("f.txt", []string{hunks[0].Hash, hunks[1].Hash}); err != nil {
		t.Fatal(err)
	}
	if got := readWT(t, dir, "f.txt"); got != numbered(nil) {
		t.Fatalf("rejecting all hunks must restore baseline, got:\n%s", got)
	}
}

func TestApproveHunks_StaleHashRefused(t *testing.T) {
	r, dir, _ := twoHunkRepo(t)
	current := numbered(map[int]string{2: "X02", 19: "X19"})
	if err := r.ApproveHunks("f.txt", []string{"deadbeefdead"}, ""); err == nil {
		t.Fatal("expected an error for an unknown hunk hash")
	}
	if got := readWT(t, dir, "f.txt"); got != current {
		t.Fatalf("a refused approve must not touch the worktree, got:\n%s", got)
	}
}
