package gitrepo

import (
	"strings"
	"testing"
)

// The reported bug: an agent CREATES a file, then EDITS it; the Changes diff
// must show real deletions vs the file's FIRST version — not the whole current
// file as one big addition with the deletion counter stuck at 0.
func TestOrigin_EditShowsDeletions(t *testing.T) {
	r, dir := fresh(t)

	// v1 — brand-new file: a clean add (all +, no -).
	writeFile(t, dir, "f.txt", "alpha\nbeta\ngamma\n")
	if _, ins, del, _ := r.FileDiff("f.txt"); ins != 3 || del != 0 {
		t.Fatalf("new file: want +3/-0, got +%d/-%d", ins, del)
	}

	// v2 — edit: drop 'beta', rename 'gamma'->'GAMMA'. Real +/- vs v1.
	writeFile(t, dir, "f.txt", "alpha\nGAMMA\n")
	diff, ins, del, err := r.FileDiff("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if ins != 1 || del != 2 {
		t.Fatalf("edit numstat = +%d/-%d, want +1/-2\n%s", ins, del, diff)
	}
	for _, want := range []string{"-beta", "-gamma", "+GAMMA"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff missing %q:\n%s", want, diff)
		}
	}
}

// The first-seen baseline is persisted, so the edit diff stays correct after a
// daemon restart mid-session.
func TestOrigin_DurableAcrossReopen(t *testing.T) {
	r, dir := fresh(t)
	writeFile(t, dir, "f.txt", "one\ntwo\nthree\n")
	if _, _, _, err := r.FileDiff("f.txt"); err != nil { // snapshots origin = v1
		t.Fatal(err)
	}

	r2, err := Open(dir) // reopen on the same workdir (daemon restart)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "f.txt", "one\nthree\n") // drop 'two'
	diff, ins, del, err := r2.FileDiff("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if del != 1 || !strings.Contains(diff, "-two") {
		t.Fatalf("after reopen the edit must still diff vs the first version (-two), got +%d/-%d\n%s", ins, del, diff)
	}
}

// History lists a file's committed revisions (the "Approval history" tab),
// oldest first, with the line delta vs the previous revision and the size.
func TestHistory_CommittedRevisions(t *testing.T) {
	r, dir := fresh(t)
	if h, _ := r.History("f.txt"); len(h) != 0 {
		t.Fatalf("no history before any commit, got %d", len(h))
	}
	writeFile(t, dir, "f.txt", "a\nb\nc\n")
	if _, err := r.Commit("rev1", []string{"f.txt"}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "f.txt", "a\nc\n") // drop 'b'
	if _, err := r.Commit("rev2", []string{"f.txt"}); err != nil {
		t.Fatal(err)
	}

	h, err := r.History("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 2 {
		t.Fatalf("want 2 revisions, got %d", len(h))
	}
	if h[0].Revision != 1 || h[0].InsDelta != 3 || h[0].DelDelta != 0 {
		t.Fatalf("rev1 wrong: %+v", h[0])
	}
	if h[1].Revision != 2 || h[1].InsDelta != 0 || h[1].DelDelta != 1 || h[1].Bytes != 4 {
		t.Fatalf("rev2 wrong: %+v", h[1])
	}
}

// After approve + commit, the baseline becomes HEAD: a later edit diffs vs the
// committed version (still shows deletions) and the origin snapshot is pruned.
func TestOrigin_ClearedAfterCommit(t *testing.T) {
	r, dir := fresh(t)
	writeFile(t, dir, "f.txt", "a\nb\nc\n")
	if _, _, _, err := r.FileDiff("f.txt"); err != nil { // snapshot origin
		t.Fatal(err)
	}
	if _, err := r.Commit("ship", []string{"f.txt"}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "f.txt", "a\nc\n") // drop 'b' after commit
	diff, _, del, err := r.FileDiff("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if del != 1 || !strings.Contains(diff, "-b") {
		t.Fatalf("post-commit edit must diff vs HEAD (-b), got -%d\n%s", del, diff)
	}
}
