package gitrepo

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// wf is a goroutine-safe file writer (t.Fatal is not safe off the test
// goroutine, so it returns an error instead).
func wf(dir, rel, content string) error {
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}

// The real 100K scenario: each session is its OWN workdir/repo. Many at once
// must be fully isolated and race-free (run the suite with -race).
func TestConcurrent_DifferentWorkdirsIsolated(t *testing.T) {
	const N = 24
	base := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dir := filepath.Join(base, fmt.Sprintf("ws%02d", i))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				errs <- err
				return
			}
			r, err := Open(dir)
			if err != nil {
				errs <- fmt.Errorf("ws%d open: %w", i, err)
				return
			}
			if _, err := r.EnsureBaseline(); err != nil {
				errs <- fmt.Errorf("ws%d baseline: %w", i, err)
				return
			}
			name := fmt.Sprintf("only_%02d.txt", i)
			if err := wf(dir, name, fmt.Sprintf("content of ws %d\n", i)); err != nil {
				errs <- err
				return
			}
			ch, err := r.Changes()
			if err != nil {
				errs <- fmt.Errorf("ws%d changes: %w", i, err)
				return
			}
			if len(ch) != 1 || ch[0].Path != name || ch[0].Status != "added" {
				errs <- fmt.Errorf("ws%d cross-talk or wrong set: %+v", i, ch)
				return
			}
			if _, ins, _, err := r.FileDiff(name); err != nil || ins != 1 {
				errs <- fmt.Errorf("ws%d diff ins=%d err=%v", i, ins, err)
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// Many concurrent reads on the SAME Repo must be race-free (the internal mutex
// serialises go-git's non-reentrant worktree ops).
func TestConcurrent_ParallelReadsSameRepo(t *testing.T) {
	r, dir := fresh(t)
	if err := wf(dir, "x.txt", "a\nb\nc\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("base", nil); err != nil {
		t.Fatal(err)
	}
	if err := wf(dir, "x.txt", "a\nB\nc\n"); err != nil {
		t.Fatal(err)
	}

	const G = 50
	var wg sync.WaitGroup
	errs := make(chan error, G)
	for g := 0; g < G; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ch, err := r.Changes(); err != nil || len(ch) != 1 || ch[0].Status != "modified" {
				errs <- fmt.Errorf("changes: err=%v %+v", err, ch)
				return
			}
			if _, ins, del, err := r.FileDiff("x.txt"); err != nil || ins != 1 || del != 1 {
				errs <- fmt.Errorf("diff: ins=%d del=%d err=%v", ins, del, err)
				return
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
