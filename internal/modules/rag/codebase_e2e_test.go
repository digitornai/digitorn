//go:build onnx && treesitter

package rag

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/embeddings"
	"github.com/digitornai/digitorn/internal/indexer"
)

// End-to-end : index a REAL code repository (the internal/codeast package)
// via the codebase connector (symbol-level tree-sitter chunks) → embed with
// minilm → store in Qdrant → semantic code search returns the right symbol.
//
//	ONNXRUNTIME_LIB=bin/onnxruntime.dll QDRANT_URL=localhost:6334 \
//	  go test -tags "onnx treesitter" ./internal/modules/rag/ -run TestRAG_CodebaseIndex_E2E -v
func TestRAG_CodebaseIndex_E2E(t *testing.T) {
	url := os.Getenv("QDRANT_URL")
	if url == "" {
		t.Skip("set QDRANT_URL to run the codebase-index E2E")
	}
	repo := "../../codeast" // the real internal/codeast Go package
	if _, err := os.Stat(repo); err != nil {
		t.Skipf("repo dir not found: %v", err)
	}
	mgr := embeddings.NewManager("", embeddings.ModeONNX, false, nil)
	defer mgr.Close()

	cfg, _ := ParseConfig(map[string]any{
		"embedding_model": "minilm-l12",
		"backend":         map[string]any{"type": "qdrant", "url": url},
		"pipeline":        map[string]any{"retrieval": "semantic"},
	})
	be, err := newBackend(cfg)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	defer be.Close()
	_ = be.DeleteKB(context.Background(), "code")

	eng := NewEngine(cfg, be, onnxEmbedder{mgr: mgr}, nil)
	svc := indexer.NewService(nil, 4)
	spec := codebaseSpec(SourceConfig{Name: "codeast", Type: "codebase", Path: repo, KnowledgeBase: "code"}, AutoIndex{})

	rep, err := svc.Sync(context.Background(), spec, ragSink{eng: eng})
	if err != nil {
		t.Fatalf("codebase sync: %v", err)
	}
	t.Logf("indexed code symbols: added=%d", rep.Added)
	if rep.Added == 0 {
		t.Fatal("indexed 0 code chunks")
	}

	hits, err := eng.Query(context.Background(), "code", "parse a source file into its symbol definitions", 3)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no code hits")
	}
	top := hits[0]
	snippet := top.Text
	if len(snippet) > 160 {
		snippet = snippet[:160]
	}
	t.Logf("TOP CODE HIT  source=%s  score=%.3f\n  %s", top.Source, top.Score, snippet)
	if !strings.Contains(strings.ToLower(top.Text), "parse") && !strings.Contains(strings.ToLower(top.Text), "chunk") {
		t.Errorf("top hit doesn't look like the parse/chunk symbol: %q", snippet)
	}
	_ = be.DeleteKB(context.Background(), "code")
}
