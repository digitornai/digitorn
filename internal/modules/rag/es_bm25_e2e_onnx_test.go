//go:build onnx

package rag

import (
	"context"
	"testing"

	"github.com/digitornai/digitorn/internal/embeddings"
)

// Proves the memory lever : with the Elasticsearch backend, hybrid retrieval's
// keyword side is served by ES's NATIVE BM25 — so the engine builds NO in-RAM
// keyword index (the corpus is not pinned in daemon RAM), yet hybrid search
// still returns the right document.
func TestRAG_ESBackend_NativeBM25_BoundedMemory(t *testing.T) {
	esURL := envD("ES_URL", "http://localhost:9200")
	ctx := context.Background()

	mgr := embeddings.NewManager("", embeddings.ModeONNX, false, nil)
	defer mgr.Close()
	cfg, _ := ParseConfig(map[string]any{
		"embedding_model": "minilm-l12",
		"backend":         map[string]any{"type": "elasticsearch", "url": esURL},
		// hybrid (default): semantic + BM25
	})
	be, err := newBackend(cfg)
	if err != nil {
		t.Skipf("no es: %v", err)
	}
	defer be.Close()
	if _, err := be.ListKBs(ctx); err != nil {
		t.Skipf("es unreachable: %v", err)
	}
	_ = be.DeleteKB(ctx, "eskb")
	eng := NewEngine(cfg, be, onnxEmbedder{mgr: mgr}, nil)

	docs := []struct{ id, text string }{
		{"1", "The quarterly financial reconciliation procedure for the ZX9 ledger system."},
		{"2", "How to replace the hydraulic seal on the TR450 excavator arm assembly."},
		{"3", "Onboarding checklist for new backend engineers joining the platform team."},
		{"4", "Troubleshooting intermittent packet loss on the edge router firmware."},
	}
	for _, d := range docs {
		if _, err := eng.IngestWithMeta(ctx, "eskb", d.text, d.id, nil); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}

	// MEMORY: the in-RAM BM25 index must be EMPTY — keyword search is delegated
	// to Elasticsearch, so a huge corpus never pins its text in daemon RAM.
	if n := eng.indexFor("eskb").length(); n != 0 {
		t.Fatalf("in-RAM BM25 index holds %d docs — ES backend should delegate keyword search server-side", n)
	}

	// Hybrid retrieval still works : a query with a distinctive keyword (TR450)
	// returns the right doc via ES native BM25 fused with semantic.
	hits, err := eng.Query(ctx, "eskb", "TR450 excavator hydraulic seal replacement", 3)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(hits) == 0 || hits[0].Source != "2" {
		t.Fatalf("hybrid+ES-BM25 wrong top hit: %+v", hits)
	}
	t.Logf("ES native BM25: hybrid query → top hit doc %s; in-RAM keyword index EMPTY (daemon memory bounded)", hits[0].Source)
	_ = be.DeleteKB(ctx, "eskb")
}
