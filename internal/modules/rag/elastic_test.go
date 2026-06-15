package rag

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestEsIndex(t *testing.T) {
	cases := map[string]string{
		"docs":        "kb_docs",
		"My KB!":      "kb_my_kb_",
		"Tickets-2024": "kb_tickets-2024",
	}
	for in, want := range cases {
		if got := esIndex(in); got != want {
			t.Errorf("esIndex(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEsFilters(t *testing.T) {
	if got := esFilters(Filter{}); got != nil {
		t.Errorf("empty filter → %v, want nil", got)
	}
	got := esFilters(Filter{Must: map[string][]string{"owner": {"alice", "bob"}}})
	want := []map[string]any{{"terms": map[string]any{"meta.owner": []string{"alice", "bob"}}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("esFilters = %v, want %v", got, want)
	}
}

func TestParseHits(t *testing.T) {
	body := []byte(`{"hits":{"hits":[
		{"_id":"a","_score":1.9,"_source":{"text":"alpha","source":"s1","chunk":0,"meta":{"owner":"alice"}}},
		{"_id":"b","_score":1.2,"_source":{"text":"beta","source":"s2","chunk":3}}
	]}}`)
	hits, err := parseHits(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].ID != "a" || hits[0].Score != 1.9 || hits[0].Source != "s1" {
		t.Fatalf("parseHits = %+v", hits)
	}
	if hits[1].Chunk != 3 {
		t.Errorf("chunk parse: %+v", hits[1])
	}
}

// TestElasticBackend_Live proves the full VectorBackend contract on a real
// Elasticsearch : index creation, bulk upsert, kNN search, ACL filter applied
// INSIDE the kNN query, count, scan, and delete-by-source. Skips if ES is
// unreachable.
func TestElasticBackend_Live(t *testing.T) {
	url := os.Getenv("ES_URL")
	if url == "" {
		url = "http://localhost:9200"
	}
	be, err := newElasticBackend(Backend{Type: "elasticsearch", URL: url})
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := be.ListKBs(ctx); err != nil {
		t.Skipf("no Elasticsearch at %s: %v", url, err)
	}

	const kb = "estest"
	_ = be.DeleteKB(ctx, kb)
	if err := be.EnsureKB(ctx, kb, 4); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	docs := []Document{
		{ID: "a", Vector: []float32{1, 0, 0, 0}, Text: "alpha apple", Source: "s1", Chunk: 0, Meta: map[string]any{"owner": "alice"}},
		{ID: "b", Vector: []float32{0, 1, 0, 0}, Text: "beta banana", Source: "s2", Chunk: 0, Meta: map[string]any{"owner": "bob"}},
		{ID: "c", Vector: []float32{0, 0, 1, 0}, Text: "gamma grape", Source: "s1", Chunk: 1, Meta: map[string]any{"owner": "alice"}},
	}
	if err := be.Upsert(ctx, kb, docs); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if n, _ := be.CountKB(ctx, kb); n != 3 {
		t.Fatalf("count = %d, want 3", n)
	}

	// kNN search near 'a'.
	hits, err := be.Search(ctx, kb, []float32{0.9, 0.1, 0, 0}, 2, Filter{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 || hits[0].ID != "a" {
		t.Fatalf("top hit = %+v, want a", hits)
	}

	// ACL filter inside the kNN query : owner=bob → only b.
	hits, err = be.Search(ctx, kb, []float32{0.9, 0.1, 0, 0}, 3, Filter{Must: map[string][]string{"owner": {"bob"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "b" {
		t.Fatalf("ACL filter failed: %+v, want only b", hits)
	}

	// Scan returns every doc (cold-start BM25 rebuild).
	if all, _ := be.Scan(ctx, kb); len(all) != 3 {
		t.Fatalf("scan = %d, want 3", len(all))
	}

	// Delete-by-source removes a + c (both s1), leaving b.
	if err := be.DeleteBySource(ctx, kb, "s1"); err != nil {
		t.Fatalf("delete_by_source: %v", err)
	}
	if n, _ := be.CountKB(ctx, kb); n != 1 {
		t.Fatalf("after delete count = %d, want 1", n)
	}
	_ = be.DeleteKB(ctx, kb)
}
