package gitrepo

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestParanoid_ConcurrentWritesSameRepo hammers one shadow repo with concurrent
// commits + status reads from many goroutines. The per-repo mutex must serialise
// the writers so go-git's index/refs never corrupt — proven by a clean `git
// fsck` and a fully-committed tree afterwards. Run with -race.
func TestParanoid_ConcurrentWritesSameRepo(t *testing.T) {
	dir := t.TempDir()
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.EnsureBaseline(); err != nil {
		t.Fatal(err)
	}

	const n = 50
	var wg sync.WaitGroup
	errCh := make(chan error, n*2)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("f_%02d.txt", i)
			if err := os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("content %d\n", i)), 0o644); err != nil {
				errCh <- err
				return
			}
			if _, err := r.Commit("add "+name, []string{name}); err != nil {
				errCh <- fmt.Errorf("commit %s: %w", name, err)
				return
			}
			// Interleave a read with the writes to exercise the lock both ways.
			if _, err := r.Changes(); err != nil {
				errCh <- fmt.Errorf("changes: %w", err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Errorf("concurrent op failed: %v", e)
	}

	// Every file committed → working tree is clean against HEAD.
	ch, err := r.Changes()
	if err != nil {
		t.Fatalf("final changes: %v", err)
	}
	if len(ch) != 0 {
		t.Fatalf("expected a clean tree after all commits, still pending: %+v", ch)
	}

	// Cross-validate integrity with the real git: fsck must be clean (non-zero
	// exit would mean a corrupt object/ref, which runGit turns into a fatal).
	gitDir := filepath.Join(dir, ".digitorn", "git")
	runGit(t, gitDir, dir, "fsck", "--full", "--strict")

	// All 50 files must be present in HEAD's tree.
	out := runGit(t, gitDir, dir, "ls-tree", "-r", "--name-only", "HEAD")
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("f_%02d.txt", i)
		if !containsLine(out, name) {
			t.Errorf("file %s missing from HEAD tree", name)
		}
	}
}

func containsLine(out, want string) bool {
	for _, line := range splitLinesTrim(out) {
		if line == want {
			return true
		}
	}
	return false
}

func splitLinesTrim(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' || r == '\r' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// TestParanoid_CorruptShadowNeverPanics proves a damaged shadow repo degrades to
// a clean error, never a panic — a corrupt .digitorn/git can't take the daemon
// (or its worker) down.
func TestParanoid_CorruptShadowNeverPanics(t *testing.T) {
	// Case 1 : garbage HEAD on open.
	t.Run("garbage_head_on_open", func(t *testing.T) {
		dir := t.TempDir()
		r, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.EnsureBaseline(); err != nil {
			t.Fatal(err)
		}
		gitDir := filepath.Join(dir, ".digitorn", "git")
		if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/\x00\x00garbage"), 0o644); err != nil {
			t.Fatal(err)
		}
		assertNoPanic(t, func() {
			if r2, err := Open(dir); err == nil && r2 != nil {
				_, _ = r2.Changes() // may error; must not panic
			}
		})
	})

	// Case 2 : objects dir wiped after a commit.
	t.Run("objects_wiped", func(t *testing.T) {
		dir := t.TempDir()
		r, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.EnsureBaseline(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Commit("a", []string{"a.txt"}); err != nil {
			t.Fatal(err)
		}
		gitDir := filepath.Join(dir, ".digitorn", "git")
		_ = os.RemoveAll(filepath.Join(gitDir, "objects"))
		assertNoPanic(t, func() {
			if r2, err := Open(dir); err == nil && r2 != nil {
				_, _ = r2.Changes()
				_, _, _, _ = r2.FileDiff("a.txt")
				_, _ = r2.Commit("again", []string{"a.txt"})
			}
		})
	})
}

func assertNoPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("operation panicked on a corrupt shadow repo: %v", rec)
		}
	}()
	fn()
}
