//go:build onnx

package rag

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/embeddings"
	"github.com/digitornai/digitorn/internal/indexer"
)

// End-to-end : crawl a REAL website (qdrant docs) via the indexation service
// (Colly) → chunk+embed with REAL minilm → store in REAL Qdrant → semantic
// query returns a page from the crawled site. This is "index a whole site,
// then ask a question and retrieve from it", proven live.
//
//	docker run -p 6333:6333 -p 6334:6334 qdrant/qdrant
//	ONNXRUNTIME_LIB=bin/onnxruntime.dll QDRANT_URL=localhost:6334 INDEXER_WEB_TEST=1 \
//	  go test -tags onnx ./internal/modules/rag/ -run TestRAG_WebIndex_E2E -v
func TestRAG_WebIndex_E2E(t *testing.T) {
	url := os.Getenv("QDRANT_URL")
	if url == "" || os.Getenv("INDEXER_WEB_TEST") == "" {
		t.Skip("set QDRANT_URL + INDEXER_WEB_TEST to run the live web-index E2E")
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
	_ = be.DeleteKB(context.Background(), "webdocs")

	eng := NewEngine(cfg, be, onnxEmbedder{mgr: mgr}, nil)

	noSitemap := false
	src := SourceConfig{
		Name: "qdrant-docs", Type: "web", KnowledgeBase: "webdocs",
		URL: "https://qdrant.tech/documentation/", MaxPages: 6, Sitemap: &noSitemap,
	}
	svc := indexer.NewService(nil, 4)
	rep, err := svc.Sync(context.Background(), webSpec(src, AutoIndex{}), ragSink{eng: eng})
	if err != nil {
		t.Fatalf("indexer sync: %v", err)
	}
	t.Logf("crawl+index report: added=%d updated=%d deleted=%d", rep.Added, rep.Updated, rep.Deleted)
	if rep.Added == 0 {
		t.Fatal("indexed 0 documents from the crawl")
	}

	hits, err := eng.Query(context.Background(), "webdocs", "how do I create a collection in qdrant", 3)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("query returned no hits from the indexed site")
	}
	top := hits[0]
	snippet := top.Text
	if len(snippet) > 140 {
		snippet = snippet[:140]
	}
	t.Logf("TOP HIT  source=%s  score=%.3f\n  %s", top.Source, top.Score, snippet)
	if !strings.Contains(top.Source, "qdrant.tech") {
		t.Errorf("top hit source %q is not from the crawled site", top.Source)
	}

	_ = be.DeleteKB(context.Background(), "webdocs")
}
