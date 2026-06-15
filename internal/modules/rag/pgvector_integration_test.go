package rag

import (
	"context"
	"os"
	"testing"

	pkgmodule "github.com/mbathepaul/digitorn/pkg/module"
)

func pgBackend(t *testing.T) VectorBackend {
	t.Helper()
	url := os.Getenv("PGVECTOR_URL")
	if url == "" {
		t.Skip("set PGVECTOR_URL (e.g. postgres://postgres:postgres@localhost:5432/postgres) to run pgvector tests")
	}
	be, err := newBackend(Config{Backend: Backend{Type: "pgvector", DSN: url}})
	if err != nil {
		t.Fatalf("pgvector backend: %v", err)
	}
	return be
}

func TestPgvector_Integration(t *testing.T) {
	be := pgBackend(t)
	defer be.Close()
	ctx := context.Background()
	kb := "pgtest"
	_ = be.DeleteKB(ctx, kb)
	if err := be.EnsureKB(ctx, kb, 4); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	docs := []Document{
		{ID: "1", Vector: []float32{1, 0, 0, 0}, Text: "alpha apple", Source: "s", Chunk: 0,
			Meta: map[string]any{"owner": "alice", "tags": []string{"x", "y"}}},
		{ID: "2", Vector: []float32{0, 1, 0, 0}, Text: "beta banana", Source: "s", Chunk: 1,
			Meta: map[string]any{"owner": "bob"}},
	}
	if err := be.Upsert(ctx, kb, docs); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := be.Upsert(ctx, kb, docs); err != nil {
		t.Fatalf("upsert re-run: %v", err)
	}
	if n, _ := be.CountKB(ctx, kb); n != 2 {
		t.Fatalf("count = %d, want 2 (re-ingest must not duplicate)", n)
	}

	hits, err := be.Search(ctx, kb, []float32{1, 0, 0, 0}, 5, Filter{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 || hits[0].ID != "1" || hits[0].Text != "alpha apple" {
		t.Fatalf("nearest = %+v, want doc 1", hits)
	}
	if hits[0].Meta == nil || hits[0].Meta["owner"] != "alice" {
		t.Errorf("meta not round-tripped: %+v", hits[0].Meta)
	}

	hits, _ = be.Search(ctx, kb, []float32{1, 0, 0, 0}, 5, Filter{Must: map[string][]string{"owner": {"bob"}}})
	if len(hits) != 1 || hits[0].ID != "2" {
		t.Fatalf("owner filter = %+v, want only doc 2 (bob), despite query nearest to doc 1", hits)
	}

	hits, _ = be.Search(ctx, kb, []float32{1, 0, 0, 0}, 5, Filter{Must: map[string][]string{"tags": {"y"}}})
	if len(hits) != 1 || hits[0].ID != "1" {
		t.Fatalf("array-tag filter = %+v, want only doc 1", hits)
	}

	if err := be.DeleteBySource(ctx, kb, "s"); err != nil {
		t.Fatalf("delete by source: %v", err)
	}
	if n, _ := be.CountKB(ctx, kb); n != 0 {
		t.Fatalf("after delete-by-source count = %d, want 0", n)
	}

	names, _ := be.ListKBs(ctx)
	found := false
	for _, n := range names {
		if n == kb {
			found = true
		}
	}
	if !found {
		t.Errorf("ListKBs missing %q: %v", kb, names)
	}
	_ = be.DeleteKB(ctx, kb)
}

func TestPgvector_ACLIsolation(t *testing.T) {
	url := os.Getenv("PGVECTOR_URL")
	if url == "" {
		t.Skip("set PGVECTOR_URL to run the pgvector ACL isolation test")
	}
	cfg, _ := ParseConfig(map[string]any{
		"backend":  map[string]any{"type": "pgvector", "dsn": url},
		"pipeline": map[string]any{"retrieval": "semantic"},
		"acl":      map[string]any{"enabled": true},
	})
	be, err := newBackend(cfg)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	defer be.Close()
	_ = be.DeleteKB(context.Background(), "aclpg")

	eng := NewEngine(cfg, be, fakeEmbedder{dim: 64}, nil)
	alice := pkgmodule.WithUserID(context.Background(), "alice")
	bob := pkgmodule.WithUserID(context.Background(), "bob")

	doc := "deployment guide how to deploy an application to the production server"
	if _, err := eng.Ingest(alice, "aclpg", doc+" alice", "alice.md"); err != nil {
		t.Fatalf("alice ingest: %v", err)
	}
	if _, err := eng.Ingest(bob, "aclpg", doc+" bob", "bob.md"); err != nil {
		t.Fatalf("bob ingest: %v", err)
	}

	aHits, err := eng.Query(alice, "aclpg", "deploy an application to the server", 10)
	if err != nil {
		t.Fatalf("alice query: %v", err)
	}
	if len(aHits) == 0 {
		t.Fatal("alice retrieved nothing")
	}
	for _, h := range aHits {
		if h.Source != "alice.md" {
			t.Errorf("ACL LEAK (pgvector vector layer): alice retrieved %q", h.Source)
		}
	}
	bHits, _ := eng.Query(bob, "aclpg", "deploy an application to the server", 10)
	if len(bHits) == 0 {
		t.Fatal("bob retrieved nothing")
	}
	for _, h := range bHits {
		if h.Source != "bob.md" {
			t.Errorf("ACL LEAK (pgvector vector layer): bob retrieved %q", h.Source)
		}
	}
	t.Logf("pgvector ACL ok: alice=%d (alice.md), bob=%d (bob.md)", len(aHits), len(bHits))
	_ = be.DeleteKB(context.Background(), "aclpg")
}
