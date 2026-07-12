package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestConcurrent_SameWorkdirSerialised drives ONE module (as the worker hosts it)
// with concurrent baseline/changes/diff/commit on the SAME workdir from many
// goroutines. The module's per-repo mutex must serialise them so the shadow git
// never corrupts. Run with -race.
func TestConcurrent_SameWorkdirSerialised(t *testing.T) {
	dir := t.TempDir()
	m := New()
	ctx := context.Background()
	// Seed a starting file so the baseline (workspace start) exists; the
	// concurrent goroutines then ADD files, which are real changes to commit.
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r, err := m.baseline(ctx, mj(map[string]any{"workdir": dir})); err != nil || !r.Success {
		t.Fatalf("baseline: %v %v", err, r.Error)
	}

	const n = 40
	var wg sync.WaitGroup
	fail := make(chan string, n*4)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("c_%02d.txt", i)
			if err := os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("v%d\n", i)), 0o644); err != nil {
				fail <- err.Error()
				return
			}
			if r, _ := m.changes(ctx, mj(map[string]any{"workdir": dir})); !r.Success {
				fail <- "changes: " + r.Error
			}
			if r, _ := m.diff(ctx, mj(map[string]any{"workdir": dir, "path": name})); !r.Success {
				fail <- "diff: " + r.Error
			}
			if r, _ := m.commit(ctx, mj(map[string]any{"workdir": dir, "message": "c " + name, "paths": []string{name}})); !r.Success {
				fail <- "commit: " + r.Error
			}
		}(i)
	}
	wg.Wait()
	close(fail)
	for f := range fail {
		t.Errorf("concurrent op failed: %s", f)
	}

	res, _ := m.changes(ctx, mj(map[string]any{"workdir": dir}))
	if files := filesOf(t, res.Data); len(files) != 0 {
		t.Fatalf("tree must be clean after all commits, pending: %+v", files)
	}
}

// TestConcurrent_ManyWorkdirsParallel proves distinct workdirs run through the
// module WITHOUT a shared lock (each has its own repo + mutex), and stay
// isolated — one module instance serving the whole pool concurrently.
func TestConcurrent_ManyWorkdirsParallel(t *testing.T) {
	m := New()
	ctx := context.Background()
	const n = 60
	dirs := make([]string, n)
	for i := range dirs {
		dirs[i] = t.TempDir()
	}

	var wg sync.WaitGroup
	fail := make(chan string, n*2)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dir := dirs[i]
			name := fmt.Sprintf("w%d.txt", i)
			// Scaffold the starting file, baseline it (= workspace start), then
			// EDIT it — only the edit is a change, and it stays isolated to this
			// workdir.
			if err := os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("hello %d\n", i)), 0o644); err != nil {
				fail <- err.Error()
				return
			}
			if r, _ := m.baseline(ctx, mj(map[string]any{"workdir": dir})); !r.Success {
				fail <- "baseline: " + r.Error
				return
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("hello %d edited\n", i)), 0o644); err != nil {
				fail <- err.Error()
				return
			}
			r, _ := m.changes(ctx, mj(map[string]any{"workdir": dir}))
			files := filesOf(t, r.Data)
			if len(files) != 1 || files[0].Path != name || files[0].Status != "modified" {
				fail <- fmt.Sprintf("workdir %d isolation wrong: %+v", i, files)
			}
		}(i)
	}
	wg.Wait()
	close(fail)
	for f := range fail {
		t.Errorf("%s", f)
	}

	// The repo cache holds exactly one entry per distinct workdir.
	m.mu.Lock()
	got := len(m.repos)
	m.mu.Unlock()
	if got != n {
		t.Fatalf("repo cache = %d entries, want %d (one per workdir)", got, n)
	}
}

// TestConcurrent_RepoCacheNoDuplicateUnderRace proves repo(workdir) returns the
// SAME *Repo even when many goroutines open the same workdir at once — no torn
// cache, no duplicate handles (which would defeat the per-workdir serialisation).
func TestConcurrent_RepoCacheNoDuplicateUnderRace(t *testing.T) {
	dir := t.TempDir()
	m := New()
	const n = 64
	var wg sync.WaitGroup
	got := make([]interface{}, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := m.repo(dir)
			if err != nil {
				t.Errorf("repo: %v", err)
				return
			}
			got[i] = r
		}(i)
	}
	wg.Wait()
	first := got[0]
	for i := 1; i < n; i++ {
		if got[i] != first {
			t.Fatalf("repo cache returned different *Repo instances under race (i=%d)", i)
		}
	}
}
