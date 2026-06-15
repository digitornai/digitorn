package rag

import (
	"context"
	"strings"
	"testing"
)

type dimModelEmbedder struct{}

func (dimModelEmbedder) EmbedModel(_ context.Context, model, role string, texts []string) ([][]float32, int, error) {
	dim := 64
	if strings.Contains(model, "big") {
		dim = 128
	}
	return fakeEmbedder{dim: dim}.EmbedModel(context.Background(), model, role, texts)
}

func TestMigrate_ReembedsToNewDimPreservingDocs(t *testing.T) {
	cfg, _ := ParseConfig(map[string]any{"embedding_model": "small"})
	be := newFakeBackend()
	eng := NewEngine(cfg, be, dimModelEmbedder{}, nil)
	ctx := context.Background()

	if _, err := eng.IngestWithMeta(ctx, "src", "deploy the server application to production", "deploy.md",
		map[string]any{"team": "ops"}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Ingest(ctx, "src", "a chocolate cake recipe with butter and sugar", "cake.md"); err != nil {
		t.Fatal(err)
	}
	srcCount, _ := be.CountKB(ctx, "src")
	if srcCount == 0 {
		t.Fatal("nothing ingested in src")
	}

	rep, err := eng.Migrate(ctx, "src", "dst", "big-model")
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if rep.Migrated != srcCount {
		t.Errorf("migrated %d, want %d", rep.Migrated, srcCount)
	}
	if rep.Dim != 128 {
		t.Errorf("target dim = %d, want 128 (the new model)", rep.Dim)
	}

	info, _ := be.KBInfo(ctx, "dst")
	if !info.Exists || info.Dim != 128 || info.Count != srcCount {
		t.Errorf("dst KBInfo = %+v, want dim 128 count %d", info, srcCount)
	}

	scan, _ := be.Scan(ctx, "dst")
	metaOK := false
	for _, d := range scan {
		if d.Source == "deploy.md" && d.Meta != nil && d.Meta["team"] == "ops" {
			metaOK = true
		}
	}
	if !metaOK {
		t.Error("metadata (team=ops) was not preserved through migration")
	}

	// dst is genuinely queryable with the new model (an engine configured for it).
	cfg2, _ := ParseConfig(map[string]any{"embedding_model": "big-model"})
	eng2 := NewEngine(cfg2, be, dimModelEmbedder{}, nil)
	hits, err := eng2.Query(ctx, "dst", "deploy the server application", 3)
	if err != nil {
		t.Fatalf("query migrated KB: %v", err)
	}
	if len(hits) == 0 || hits[0].Source != "deploy.md" {
		t.Fatalf("migrated KB query top = %+v, want deploy.md", hits)
	}
}
