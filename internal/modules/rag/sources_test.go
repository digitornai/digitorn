package rag

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSyncSource_IncrementalAddUpdateDelete(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.md", "alpha document")
	write("b.md", "beta document")

	cfg, _ := ParseConfig(nil)
	be := newFakeBackend()
	eng := NewEngine(cfg, be, fakeEmbedder{dim: 64}, nil)
	ctx := context.Background()
	src := SourceConfig{Type: "file", Path: dir, KnowledgeBase: "kb"}

	// First sync: both added.
	rep, err := eng.SyncSource(ctx, src)
	if err != nil {
		t.Fatalf("sync1: %v", err)
	}
	if rep.Added != 2 || rep.Updated != 0 || rep.Deleted != 0 {
		t.Fatalf("sync1 report = %+v, want Added=2", rep)
	}

	// Re-sync unchanged: no-op.
	rep, _ = eng.SyncSource(ctx, src)
	if rep.Added+rep.Updated+rep.Deleted != 0 {
		t.Fatalf("re-sync unchanged should be no-op, got %+v", rep)
	}

	// Modify a.md → Updated (bump mtime to ensure the stat-sig changes).
	time.Sleep(10 * time.Millisecond)
	write("a.md", "alpha document revised with more text")
	rep, _ = eng.SyncSource(ctx, src)
	if rep.Updated != 1 || rep.Added != 0 {
		t.Fatalf("after edit report = %+v, want Updated=1", rep)
	}

	// Delete b.md → Deleted, and its chunks removed from the backend.
	if err := os.Remove(filepath.Join(dir, "b.md")); err != nil {
		t.Fatal(err)
	}
	rep, _ = eng.SyncSource(ctx, src)
	if rep.Deleted != 1 {
		t.Fatalf("after delete report = %+v, want Deleted=1", rep)
	}
	if n, _ := be.CountKB(ctx, "kb"); n == 0 {
		t.Fatal("kb empty after delete (a.md should remain)")
	}
	// b.md must be gone from the backend.
	for _, d := range be.docs["kb"] {
		if d.Source == "b.md" {
			t.Fatal("b.md chunks still present after delete")
		}
	}
}
