package rag

import (
	"context"
	"testing"

	pkgmodule "github.com/digitornai/digitorn/pkg/module"
)

func TestACL_FilterFirst_PerUserIsolation(t *testing.T) {
	cfg, _ := ParseConfig(map[string]any{"acl": map[string]any{"enabled": true}})
	eng := NewEngine(cfg, newFakeBackend(), fakeEmbedder{dim: 64}, nil)

	alice := pkgmodule.WithUserID(context.Background(), "alice")
	bob := pkgmodule.WithUserID(context.Background(), "bob")

	doc := "deployment guide explains how to deploy an application to the production server"
	if _, err := eng.Ingest(alice, "kb", doc+" alice-only", "alice.md"); err != nil {
		t.Fatalf("alice ingest: %v", err)
	}
	if _, err := eng.Ingest(bob, "kb", doc+" bob-only", "bob.md"); err != nil {
		t.Fatalf("bob ingest: %v", err)
	}

	aHits, err := eng.Query(alice, "kb", "how to deploy an application to the server", 10)
	if err != nil {
		t.Fatalf("alice query: %v", err)
	}
	if len(aHits) == 0 {
		t.Fatal("alice retrieved nothing")
	}
	for _, h := range aHits {
		if h.Source != "alice.md" {
			t.Errorf("ACL LEAK: alice retrieved %q (must be alice.md only)", h.Source)
		}
	}

	bHits, err := eng.Query(bob, "kb", "how to deploy an application to the server", 10)
	if err != nil {
		t.Fatalf("bob query: %v", err)
	}
	if len(bHits) == 0 {
		t.Fatal("bob retrieved nothing")
	}
	for _, h := range bHits {
		if h.Source != "bob.md" {
			t.Errorf("ACL LEAK: bob retrieved %q (must be bob.md only)", h.Source)
		}
	}
}

func TestACL_Disabled_ReturnsAll(t *testing.T) {
	cfg, _ := ParseConfig(nil)
	eng := NewEngine(cfg, newFakeBackend(), fakeEmbedder{dim: 64}, nil)
	alice := pkgmodule.WithUserID(context.Background(), "alice")
	bob := pkgmodule.WithUserID(context.Background(), "bob")
	doc := "deployment guide deploy application production server"
	_, _ = eng.Ingest(alice, "kb", doc+" one", "a.md")
	_, _ = eng.Ingest(bob, "kb", doc+" two", "b.md")

	hits, _ := eng.Query(alice, "kb", "deploy application server", 10)
	srcs := map[string]bool{}
	for _, h := range hits {
		srcs[h.Source] = true
	}
	if !srcs["a.md"] || !srcs["b.md"] {
		t.Errorf("ACL disabled must return both owners: got %v", srcs)
	}
}

func TestACL_OwnerIsAuthoritative(t *testing.T) {
	cfg, _ := ParseConfig(map[string]any{"acl": map[string]any{"enabled": true}})
	eng := NewEngine(cfg, newFakeBackend(), fakeEmbedder{dim: 64}, nil)
	alice := pkgmodule.WithUserID(context.Background(), "alice")

	// Alice tries to forge owner=bob via author metadata — the engine must
	// overwrite it with her real identity.
	if _, err := eng.IngestWithMeta(alice, "kb", "secret deployment notes", "x.md",
		map[string]any{"owner": "bob"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	// Bob must NOT be able to see it.
	bob := pkgmodule.WithUserID(context.Background(), "bob")
	hits, _ := eng.Query(bob, "kb", "deployment notes", 10)
	if len(hits) != 0 {
		t.Errorf("forged owner leaked the doc to bob: %d hits", len(hits))
	}
	// Alice can.
	aHits, _ := eng.Query(alice, "kb", "deployment notes", 10)
	if len(aHits) == 0 {
		t.Error("alice cannot see her own doc")
	}
}
