package rag

import (
	"context"
	"os"
	"testing"
)

func TestQdrant_Integration(t *testing.T) {
	url := os.Getenv("QDRANT_URL")
	if url == "" {
		t.Skip("set QDRANT_URL (e.g. localhost:6334) to run against a live Qdrant")
	}
	be, err := newQdrantBackend(Backend{URL: url})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer be.Close()

	ctx := context.Background()
	const kb = "digitorn_rag_itest"
	_ = be.DeleteKB(ctx, kb)

	if err := be.EnsureKB(ctx, kb, 4); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := be.EnsureKB(ctx, kb, 4); err != nil {
		t.Fatalf("ensure idempotent: %v", err)
	}

	docs := []Document{
		{ID: docID(kb, "s", 0), Vector: []float32{1, 0, 0, 0}, Text: "alpha apple", Source: "s", Chunk: 0},
		{ID: docID(kb, "s", 1), Vector: []float32{0, 1, 0, 0}, Text: "beta banana", Source: "s", Chunk: 1},
	}
	if err := be.Upsert(ctx, kb, docs); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := be.Upsert(ctx, kb, docs); err != nil {
		t.Fatalf("upsert re-run: %v", err)
	}

	count, err := be.CountKB(ctx, kb)
	if err != nil || count != 2 {
		t.Fatalf("count = %d, err %v (want 2 — re-ingest must not duplicate)", count, err)
	}

	hits, err := be.Search(ctx, kb, []float32{1, 0, 0, 0}, 2, Filter{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 || hits[0].Text != "alpha apple" {
		t.Fatalf("nearest hit = %+v, want 'alpha apple'", hits)
	}
	if hits[0].Source != "s" || hits[0].Chunk != 0 {
		t.Errorf("citation payload lost: source=%q chunk=%d", hits[0].Source, hits[0].Chunk)
	}

	names, err := be.ListKBs(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !contains(names, kb) {
		t.Errorf("kb %q not in list %v", kb, names)
	}

	if err := be.DeleteKB(ctx, kb); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
