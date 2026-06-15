//go:build onnx

package rag

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/embeddings"
)

// End-to-end : N distinct apps index their own documents CONCURRENTLY with REAL
// minilm embeddings into their own knowledge bases on REAL Qdrant, then each
// app retrieves ONLY its own data. Proves the full pipeline holds up under many
// concurrent tenants with correct per-app isolation — the "centaines d'apps"
// case at full-pipeline scale (N kept runnable).
func TestRAG_MultiTenant_Concurrent_E2E(t *testing.T) {
	qurl := os.Getenv("QDRANT_URL")
	if qurl == "" {
		qurl = "localhost:6334"
	}
	const N = 30

	mgr := embeddings.NewManager("", embeddings.ModeONNX, false, nil)
	defer mgr.Close()
	cfg, _ := ParseConfig(map[string]any{
		"embedding_model": "minilm-l12",
		"backend":         map[string]any{"type": "qdrant", "url": qurl},
		"pipeline":        map[string]any{"retrieval": "semantic"},
	})
	be, err := newBackend(cfg)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	defer be.Close()
	eng := NewEngine(cfg, be, onnxEmbedder{mgr: mgr}, nil)

	kb := func(i int) string { return fmt.Sprintf("tenant_%d", i) }
	for i := 0; i < N; i++ {
		_ = be.DeleteKB(context.Background(), kb(i))
	}

	// Concurrent indexing : each tenant writes a signature doc + filler.
	start := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			sig := fmt.Sprintf("Customer support record TENANT-%d: account recovery and billing hotline for premium members.", i)
			if _, err := eng.Ingest(ctx, kb(i), sig, "support"); err != nil {
				errs[i] = err
				return
			}
			if _, err := eng.Ingest(ctx, kb(i), "General terms and conditions of service.", "terms"); err != nil {
				errs[i] = err
				return
			}
			if _, err := eng.Ingest(ctx, kb(i), "Shipping and returns policy for online orders.", "shipping"); err != nil {
				errs[i] = err
			}
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("tenant %d index failed: %v", i, e)
		}
	}
	t.Logf("indexed %d tenants concurrently (%d docs) in %v", N, N*3, time.Since(start).Round(time.Millisecond))

	// Each tenant retrieves ONLY its own record.
	for i := 0; i < N; i++ {
		hits, err := eng.Query(context.Background(), kb(i), "how do I recover my account and reach billing", 1)
		if err != nil {
			t.Fatalf("tenant %d query: %v", i, err)
		}
		if len(hits) == 0 {
			t.Fatalf("tenant %d got no hits", i)
		}
		want := fmt.Sprintf("TENANT-%d:", i)
		if !strings.Contains(hits[0].Text, want) {
			t.Fatalf("tenant %d isolation/retrieval failed: top hit = %q, want marker %q", i, hits[0].Text, want)
		}
	}
	t.Logf("all %d tenants retrieved their own record — isolation holds", N)

	for i := 0; i < N; i++ {
		_ = be.DeleteKB(context.Background(), kb(i))
	}
}
