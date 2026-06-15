package indexer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A web source pointing at a loopback/private address must be refused by the
// crawler's SSRF guard (the httptest server binds 127.0.0.1), and must succeed
// only when the operator explicitly opts in with allow_private.
func TestWebConnector_SSRFGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><head><title>secret</title></head><body>internal metadata</body></html>"))
	}))
	defer srv.Close()

	count := func(opts map[string]any) int {
		var c webConnector
		spec := SourceSpec{Name: "t", Type: "web", KB: "kb", Opts: opts}
		n := 0
		_ = c.Walk(context.Background(), spec, func(Document) error { n++; return nil })
		return n
	}

	if got := count(map[string]any{"url": srv.URL, "sitemap": false}); got != 0 {
		t.Fatalf("guard OFF leak: crawled %d loopback pages, want 0 (SSRF blocked)", got)
	}
	if got := count(map[string]any{"url": srv.URL, "sitemap": false, "allow_private": true}); got == 0 {
		t.Fatalf("allow_private opt-in failed: crawled 0 pages, want >=1")
	}
}
