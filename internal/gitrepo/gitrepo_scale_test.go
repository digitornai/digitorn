package gitrepo

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Status/diff over a realistic project tree must stay quick enough for the
// debounced background refresh (these run in the worker, off the hot path).
func TestScale_ManyFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test skipped in -short")
	}
	r, dir := fresh(t)

	const N = 300
	for i := 0; i < N; i++ {
		if err := wf(dir, fmt.Sprintf("pkg/m%02d/file_%03d.go", i%10, i),
			fmt.Sprintf("package m\n\nfunc F%03d() int { return %d }\n", i, i)); err != nil {
			t.Fatal(err)
		}
	}

	start := time.Now()
	ch, err := r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Changes over %d new files: %d found in %s", N, len(ch), time.Since(start))
	if len(ch) != N {
		t.Fatalf("expected %d changes, got %d", N, len(ch))
	}
	if _, err := r.Commit("baseline", nil); err != nil {
		t.Fatal(err)
	}

	const M = 30
	for i := 0; i < M; i++ {
		if err := wf(dir, fmt.Sprintf("pkg/m%02d/file_%03d.go", i%10, i),
			fmt.Sprintf("package m\n\nfunc F%03d() int { return %d /* edited */ }\n", i, i*2)); err != nil {
			t.Fatal(err)
		}
	}
	start = time.Now()
	ch, _ = r.Changes()
	t.Logf("Changes after editing %d/%d: %d found in %s", M, N, len(ch), time.Since(start))
	if len(ch) != M {
		t.Fatalf("expected %d modified, got %d", M, len(ch))
	}
}

// A single very large file diff must compute the right numstat and stay fast.
func TestScale_LargeFileDiff(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test skipped in -short")
	}
	r, dir := fresh(t)
	const lines = 5000
	if err := wf(dir, "big.go", strings.Repeat("a line of source code\n", lines)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("base", nil); err != nil {
		t.Fatal(err)
	}
	// change one line in the middle
	body := strings.Repeat("a line of source code\n", lines)
	idx := strings.Index(body[len(body)/2:], "\n") + len(body)/2 + 1
	mutated := body[:idx] + "MUTATED LINE\n" + body[idx:]
	if err := wf(dir, "big.go", mutated); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, ins, del, err := r.FileDiff("big.go")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("large-file diff (%d lines) in %s -> +%d/-%d", lines, time.Since(start), ins, del)
	if ins != 1 || del != 0 {
		t.Fatalf("large-file numstat = %d/%d, want 1/0", ins, del)
	}
}

func BenchmarkStatus(b *testing.B) {
	dir := b.TempDir()
	r, _ := Open(dir)
	r.EnsureBaseline()
	for i := 0; i < 100; i++ {
		wf(dir, fmt.Sprintf("f%03d.txt", i), "content\n")
	}
	r.Commit("base", nil)
	wf(dir, "f000.txt", "changed\n")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Changes(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFileDiff(b *testing.B) {
	dir := b.TempDir()
	r, _ := Open(dir)
	r.EnsureBaseline()
	wf(dir, "big.go", strings.Repeat("line of code\n", 500))
	r.Commit("base", nil)
	wf(dir, "big.go", strings.Repeat("line of code\n", 250)+"CHANGED\n"+strings.Repeat("line of code\n", 249))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, _, err := r.FileDiff("big.go"); err != nil {
			b.Fatal(err)
		}
	}
}
