package gitrepo

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestApprove_StagesFile(t *testing.T) {
	dir := t.TempDir()
	r, _ := Open(dir)
	if _, err := r.EnsureBaseline(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "a.txt", "hi\n")

	ch, _ := r.Changes()
	if len(ch) != 1 || ch[0].Path != "a.txt" || ch[0].Staged {
		t.Fatalf("before approve, expected one unstaged change, got %+v", ch)
	}
	if err := r.Stage([]string{"a.txt"}); err != nil {
		t.Fatal(err)
	}
	ch, _ = r.Changes()
	if len(ch) != 1 || !ch[0].Staged {
		t.Fatalf("after approve the file must be staged, got %+v", ch)
	}
}

func TestReject_ModifiedReverts(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "v1\n")
	r, _ := Open(dir)
	if _, err := r.EnsureBaseline(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "a.txt", "v2\n")
	if cs := changeSet(t, r); cs["a.txt"] != "modified" {
		t.Fatalf("expected modified before reject, got %v", cs)
	}
	if err := r.Restore([]string{"a.txt"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "v1\n" {
		t.Fatalf("reject must restore the baseline content, got %q", b)
	}
	if cs := changeSet(t, r); len(cs) != 0 {
		t.Fatalf("file still pending after reject: %v", cs)
	}
}

func TestReject_AddedRemoved(t *testing.T) {
	dir := t.TempDir()
	r, _ := Open(dir)
	if _, err := r.EnsureBaseline(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "new.txt", "x\n")
	// Approve it first, so reject must also clear the staged "added" index entry.
	if err := r.Stage([]string{"new.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Restore([]string{"new.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("a rejected newly-added file must be deleted, stat err=%v", err)
	}
	if cs := changeSet(t, r); len(cs) != 0 {
		t.Fatalf("file still pending after reject-added: %v", cs)
	}
}

func TestCommit_OnlyStaged(t *testing.T) {
	dir := t.TempDir()
	r, _ := Open(dir)
	if _, err := r.EnsureBaseline(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "A.txt", "a\n")
	writeFile(t, dir, "B.txt", "b\n")
	if err := r.Stage([]string{"A.txt"}); err != nil { // approve A only
		t.Fatal(err)
	}
	if _, err := r.Commit("ship A", nil); err != nil {
		t.Fatal(err)
	}
	cs := changeSet(t, r)
	if _, ok := cs["A.txt"]; ok {
		t.Fatalf("A.txt was approved+committed, must leave the pending set: %v", cs)
	}
	if cs["B.txt"] != "added" {
		t.Fatalf("B.txt was never approved, must stay pending: %v", cs)
	}
}

func TestCommit_NothingStaged(t *testing.T) {
	dir := t.TempDir()
	r, _ := Open(dir)
	if _, err := r.EnsureBaseline(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "a.txt", "x\n") // pending, NOT approved
	if _, err := r.Commit("", nil); !errors.Is(err, ErrNothingStaged) {
		t.Fatalf("commit with nothing approved must return ErrNothingStaged, got %v", err)
	}
}
