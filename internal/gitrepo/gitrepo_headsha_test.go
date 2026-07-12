package gitrepo

import "testing"

// TestHeadSHA : empty before any commit, then equal to the latest commit sha —
// this is what the GitHub push/status wiring uses to track what's been pushed.
func TestHeadSHA(t *testing.T) {
	dir := t.TempDir()
	r, _ := Open(dir)

	// Fresh repo, no commit yet → empty.
	if sha, err := r.HeadSHA(); err != nil || sha != "" {
		t.Fatalf("fresh repo HeadSHA = %q, err=%v; want empty", sha, err)
	}

	if _, err := r.EnsureBaseline(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "a.txt", "hi\n")
	if err := r.StageAll(); err != nil {
		t.Fatal(err)
	}
	sha, err := r.Commit("first", nil)
	if err != nil {
		t.Fatal(err)
	}

	head, err := r.HeadSHA()
	if err != nil || head == "" {
		t.Fatalf("after commit HeadSHA = %q, err=%v; want non-empty", head, err)
	}
	if head != sha {
		t.Fatalf("HeadSHA %s != returned commit sha %s", head, sha)
	}
	if log, _ := r.Log(); len(log) == 0 || log[0].Sha != head {
		t.Fatalf("Log[0] must equal HEAD %s, got %+v", head, log)
	}
}
