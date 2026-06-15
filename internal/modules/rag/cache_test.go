package rag

import (
	"context"
	"testing"

	pkgmodule "github.com/mbathepaul/digitorn/pkg/module"
)

type countingBackend struct {
	*fakeBackend
	searches int
}

func (c *countingBackend) Search(ctx context.Context, kb string, vec []float32, topK int, filter Filter) ([]SearchHit, error) {
	c.searches++
	return c.fakeBackend.Search(ctx, kb, vec, topK, filter)
}

func TestCache_HitAvoidsBackendAndInvalidates(t *testing.T) {
	cfg, _ := ParseConfig(map[string]any{
		"pipeline": map[string]any{"retrieval": "semantic"},
		"cache":    map[string]any{"enabled": true},
	})
	cb := &countingBackend{fakeBackend: newFakeBackend()}
	eng := NewEngine(cfg, cb, fakeEmbedder{dim: 64}, nil)
	ctx := context.Background()

	if _, err := eng.Ingest(ctx, "kb", "alpha beta deploy the server application now", "s"); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.Query(ctx, "kb", "deploy the server application", 5); err != nil {
		t.Fatal(err)
	}
	first := cb.searches
	if first == 0 {
		t.Fatal("expected a backend search on the first query")
	}

	if _, err := eng.Query(ctx, "kb", "deploy the server application", 5); err != nil {
		t.Fatal(err)
	}
	if cb.searches != first {
		t.Errorf("cache MISS: backend searched again on identical query (%d -> %d)", first, cb.searches)
	}

	if _, err := eng.Ingest(ctx, "kb", "an unrelated extra document", "s2"); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Query(ctx, "kb", "deploy the server application", 5); err != nil {
		t.Fatal(err)
	}
	if cb.searches == first {
		t.Error("invalidation FAILED: cache served a stale result after ingest")
	}
}

func TestCache_ACLIsolation(t *testing.T) {
	cfg, _ := ParseConfig(map[string]any{
		"pipeline": map[string]any{"retrieval": "semantic"},
		"cache":    map[string]any{"enabled": true},
		"acl":      map[string]any{"enabled": true},
	})
	eng := NewEngine(cfg, newFakeBackend(), fakeEmbedder{dim: 64}, nil)
	alice := pkgmodule.WithUserID(context.Background(), "alice")
	bob := pkgmodule.WithUserID(context.Background(), "bob")

	doc := "deploy the server application to production"
	_, _ = eng.Ingest(alice, "kb", doc+" alice", "alice.md")
	_, _ = eng.Ingest(bob, "kb", doc+" bob", "bob.md")

	// Prime alice's cache.
	if _, err := eng.Query(alice, "kb", "deploy the server application", 5); err != nil {
		t.Fatal(err)
	}
	// Bob's identical query must NOT be served from alice's cache entry.
	bHits, err := eng.Query(bob, "kb", "deploy the server application", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(bHits) == 0 {
		t.Fatal("bob retrieved nothing")
	}
	for _, h := range bHits {
		if h.Source != "bob.md" {
			t.Errorf("CACHE ACL LEAK: bob got %q from cache (must be bob.md only)", h.Source)
		}
	}
}

func TestDiscover_KBInfo(t *testing.T) {
	be := newFakeBackend()
	ctx := context.Background()
	if err := be.EnsureKB(ctx, "kb", 384); err != nil {
		t.Fatal(err)
	}
	_ = be.Upsert(ctx, "kb", []Document{{ID: "1", Vector: make([]float32, 384), Text: "x", Source: "s"}})

	info, _ := be.KBInfo(ctx, "kb")
	if !info.Exists || info.Dim != 384 || info.Count != 1 {
		t.Errorf("KBInfo = %+v, want {Exists:true Dim:384 Count:1}", info)
	}
	missing, _ := be.KBInfo(ctx, "nope")
	if missing.Exists {
		t.Errorf("KBInfo for absent KB must report Exists=false, got %+v", missing)
	}
}
