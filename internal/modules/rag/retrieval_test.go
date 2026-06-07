package rag

import (
	"context"
	"testing"
)

func TestBM25_RanksKeywordDoc(t *testing.T) {
	b := NewBM25()
	b.Add("a", "the quick brown fox jumps")
	b.Add("b", "kubernetes deployment rollout strategy")
	b.Add("c", "a slow green turtle walks")
	hits := b.Search("kubernetes deployment", 3)
	if len(hits) == 0 || hits[0].ID != "b" {
		t.Fatalf("BM25 top = %+v, want id b", hits)
	}
}

func TestBM25_ReAddReplaces(t *testing.T) {
	b := NewBM25()
	b.Add("x", "alpha beta")
	b.Add("x", "gamma delta")
	if b.Len() != 1 {
		t.Fatalf("len = %d, want 1 (re-add must replace)", b.Len())
	}
	if hits := b.Search("alpha", 5); len(hits) != 0 {
		t.Errorf("stale term still indexed: %+v", hits)
	}
	if hits := b.Search("gamma", 5); len(hits) != 1 {
		t.Errorf("new term not indexed: %+v", hits)
	}
}

func TestRRF_FusesAndWeights(t *testing.T) {
	sem := []string{"s1", "s2", "s3"}
	bm := []string{"b1", "s2", "b2"}
	// s2 appears in both → should rank top under equal weights.
	fused := rrfFuse([][]string{sem, bm}, []float64{1, 1}, 5)
	if len(fused) == 0 || fused[0] != "s2" {
		t.Fatalf("fused top = %v, want s2 first", fused)
	}
	// Heavily weight the bm list → its exclusive top (b1) outranks s1.
	fusedW := rrfFuse([][]string{sem, bm}, []float64{0.1, 10}, 5)
	if indexOf(fusedW, "b1") > indexOf(fusedW, "s1") {
		t.Errorf("weighting failed: %v (b1 should beat s1)", fusedW)
	}
}

func TestEngine_BM25Mode(t *testing.T) {
	cfg, _ := ParseConfig(map[string]any{"pipeline": map[string]any{"retrieval": "bm25"}})
	eng := NewEngine(cfg, newFakeBackend(), fakeEmbedder{dim: 64}, nil)
	ctx := context.Background()
	_, err := eng.Ingest(ctx, "kb", "kubernetes deployment guide. chocolate cake recipe. nightly database backups.", "doc")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	hits, err := eng.Query(ctx, "kb", "kubernetes deployment", 3)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("bm25 returned no hits")
	}
}

func TestEngine_ColdStartRebuildsKeywordIndex(t *testing.T) {
	cfg, _ := ParseConfig(map[string]any{"pipeline": map[string]any{"retrieval": "bm25"}})
	be := newFakeBackend()
	ctx := context.Background()
	// Ingest with one engine, then query with a FRESH engine (cold keyword
	// index) — it must rebuild from the backend via Scan.
	_, _ = NewEngine(cfg, be, fakeEmbedder{dim: 64}, nil).Ingest(ctx, "kb", "alpha beta gamma delta epsilon", "s")
	fresh := NewEngine(cfg, be, fakeEmbedder{dim: 64}, nil)
	hits, err := fresh.Query(ctx, "kb", "gamma", 3)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("cold-start rebuild failed: no bm25 hits after Scan")
	}
}

func indexOf(ss []string, s string) int {
	for i, x := range ss {
		if x == s {
			return i
		}
	}
	return len(ss)
}
