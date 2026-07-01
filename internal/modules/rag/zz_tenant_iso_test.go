//go:build onnx

package rag

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/embeddings"
	pkgmodule "github.com/digitornai/digitorn/pkg/module"
)

func TestProbe_CrossAppSharedKB(t *testing.T) {
	qurl := os.Getenv("QDRANT_URL")
	if qurl == "" {
		qurl = "localhost:6334"
	}
	mgr := embeddings.NewManager("", embeddings.ModeONNX, false, nil)
	defer mgr.Close()

	mkEngine := func() *Engine {
		cfg, _ := ParseConfig(map[string]any{
			"embedding_model": "minilm-l12",
			"backend":         map[string]any{"type": "qdrant", "url": qurl},
			"pipeline":        map[string]any{"retrieval": "semantic"},
		})
		be, err := newBackend(cfg)
		if err != nil {
			t.Fatalf("backend: %v", err)
		}
		return NewEngine(cfg, be, onnxEmbedder{mgr: mgr}, nil)
	}

	const kb = "shared_default"
	engA := mkEngine()
	engB := mkEngine()
	defer engA.Close()
	defer engB.Close()
	_ = engA.backend.DeleteKB(context.Background(), kb)

	ctxA := pkgmodule.WithAppID(context.Background(), "appA")
	if _, err := engA.Ingest(ctxA, kb, "APPA-SECRET: internal revenue projections Q4 confidential", "secret"); err != nil {
		t.Fatalf("appA ingest: %v", err)
	}

	ctxB := pkgmodule.WithAppID(context.Background(), "appB")
	hits, err := engB.Query(ctxB, kb, "revenue projections confidential", 3)
	if err != nil {
		t.Fatalf("appB query: %v", err)
	}
	leaked := false
	for _, h := range hits {
		if strings.Contains(h.Text, "APPA-SECRET") {
			leaked = true
		}
	}
	t.Logf("appB sees %d hits on shared KB; appA-secret leaked to appB = %v", len(hits), leaked)
	if leaked {
		t.Logf(">>> CROSS-APP DATA MIXING: app B read app A's docs from shared backend+KB. Isolation is NOT app-scoped at storage; relies on distinct DSN or KB names.")
	}
	_ = engA.backend.DeleteKB(context.Background(), kb)
}

func TestProbe_KBNameCollisionPgvector(t *testing.T) {
	dsn := os.Getenv("PGVECTOR_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5433/postgres?sslmode=disable"
	}
	mgr := embeddings.NewManager("", embeddings.ModeONNX, false, nil)
	defer mgr.Close()
	cfg, _ := ParseConfig(map[string]any{
		"embedding_model": "minilm-l12",
		"backend":         map[string]any{"type": "pgvector", "dsn": dsn},
		"pipeline":        map[string]any{"retrieval": "semantic"},
	})
	be, err := newBackend(cfg)
	if err != nil {
		t.Skipf("pgvector backend: %v", err)
	}
	defer be.Close()
	eng := NewEngine(cfg, be, onnxEmbedder{mgr: mgr}, nil)

	t.Logf("pgTable('My-KB')=%q  pgTable('my_kb')=%q  pgTable('my.kb')=%q",
		pgTable("My-KB"), pgTable("my_kb"), pgTable("my.kb"))

	_ = be.DeleteKB(context.Background(), "My-KB")
	ctx := context.Background()
	if _, err := eng.Ingest(ctx, "My-KB", "DOC-IN-MYKB-HYPHEN: alpha bravo charlie", "h"); err != nil {
		t.Fatalf("ingest My-KB: %v", err)
	}
	hits, err := eng.Query(ctx, "my_kb", "alpha bravo charlie", 3)
	if err != nil {
		t.Fatalf("query my_kb: %v", err)
	}
	mixed := false
	for _, h := range hits {
		if strings.Contains(h.Text, "DOC-IN-MYKB-HYPHEN") {
			mixed = true
		}
	}
	t.Logf("query 'my_kb' returned %d hits; doc in 'My-KB' visible = %v", len(hits), mixed)
	if mixed {
		t.Logf(">>> KB-NAME COLLISION: 'My-KB' and 'my_kb' share physical table %q.", pgTable("My-KB"))
	}
	_ = be.DeleteKB(context.Background(), "My-KB")
}
