package indexer

import (
	"context"
	"os"
	"sync"
	"testing"
)

// Live crawl of a real site. Gated so CI/offline stays green.
//
//	INDEXER_WEB_TEST=1 go test ./internal/indexer/ -run TestWebConnector_Live -v
func TestWebConnector_Live(t *testing.T) {
	if os.Getenv("INDEXER_WEB_TEST") == "" {
		t.Skip("set INDEXER_WEB_TEST=1 to run the live web crawl")
	}
	conn := &webConnector{}
	spec := SourceSpec{Name: "qd", Type: "web", KB: "docs", Opts: map[string]any{
		"url":         "https://qdrant.tech/documentation/",
		"max_pages":   12,
		"same_domain": true,
		"sitemap":     false, // link-crawl path (sitemap covered separately)
		"rate_limit":  "200ms",
		"parallelism": 4,
	}}

	var mu sync.Mutex
	var docs []Document
	if err := conn.Walk(context.Background(), spec, func(d Document) error {
		mu.Lock()
		docs = append(docs, d)
		mu.Unlock()
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(docs) < 3 {
		t.Fatalf("crawled %d pages, want >= 3", len(docs))
	}
	thin := 0
	for _, d := range docs {
		if d.ID == "" || d.Meta["url"] == nil || len(d.Text) < 50 {
			thin++
		}
	}
	if thin > len(docs)/2 {
		t.Errorf("%d/%d pages are thin (missing url/text)", thin, len(docs))
	}
	t.Logf("crawled %d pages; sample id=%q title=%v (%d chars)",
		len(docs), docs[0].ID, docs[0].Meta["title"], len(docs[0].Text))
}
