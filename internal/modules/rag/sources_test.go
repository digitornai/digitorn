package rag

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbathepaul/digitorn/internal/indexer"
)

// File sources are driven by the indexation service (Tabula connector +
// content-hash incremental diff). This proves add/update/delete through the
// real service path + the ragSink (chunk+embed+store).
func TestFileSource_IncrementalViaService(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.md", "alpha document about deployment")
	write("b.md", "beta document about backups")

	cfg, _ := ParseConfig(nil)
	be := newFakeBackend()
	eng := NewEngine(cfg, be, fakeEmbedder{dim: 64}, nil)
	svc := indexer.NewService(nil, 2)
	sink := ragSink{eng: eng}
	spec := fileSpec(SourceConfig{Type: "file", Path: dir, KnowledgeBase: "kb"}, AutoIndex{})
	ctx := context.Background()

	rep, err := svc.Sync(ctx, spec, sink)
	if err != nil {
		t.Fatalf("sync1: %v", err)
	}
	if rep.Added != 2 || rep.Updated != 0 || rep.Deleted != 0 {
		t.Fatalf("sync1 = %+v, want Added=2", rep)
	}

	if rep, _ = svc.Sync(ctx, spec, sink); rep.Added+rep.Updated+rep.Deleted != 0 {
		t.Fatalf("re-sync unchanged should be no-op, got %+v", rep)
	}

	write("a.md", "alpha document revised with more deployment text")
	if rep, _ = svc.Sync(ctx, spec, sink); rep.Updated != 1 || rep.Added != 0 {
		t.Fatalf("after edit = %+v, want Updated=1", rep)
	}

	if err := os.Remove(filepath.Join(dir, "b.md")); err != nil {
		t.Fatal(err)
	}
	if rep, _ = svc.Sync(ctx, spec, sink); rep.Deleted != 1 {
		t.Fatalf("after delete = %+v, want Deleted=1", rep)
	}
	for _, d := range be.docs["kb"] {
		if d.Source == "b.md" {
			t.Fatal("b.md chunks still present after delete")
		}
	}
}
