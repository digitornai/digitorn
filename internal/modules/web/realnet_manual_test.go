package web

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// Manual real-internet probe (opt-in so it never runs in CI / offline). Run:
//
//	WEB_REALNET=1 go test ./internal/modules/web/ -run TestRealNet -v -count=1
func TestRealNet(t *testing.T) {
	if os.Getenv("WEB_REALNET") == "" {
		t.Skip("set WEB_REALNET=1 to run the live-network probe")
	}
	m := New()
	m.apply(Config{}) // defaults: DuckDuckGo, SSRF guard ON (real public sites)

	// --- search against LIVE DuckDuckGo ---
	raw, _ := json.Marshal(map[string]any{"query": "golang context cancellation", "limit": 3})
	res, err := m.search(context.Background(), raw)
	if err != nil {
		t.Fatalf("REAL search failed: %v", err)
	}
	data := res.Data.(map[string]any)
	results := data["results"].([]searchResult)
	fmt.Printf("\n=== SEARCH backend=%v count=%d ===\n", data["backend"], len(results))
	for i, r := range results {
		fmt.Printf("[%d] %s\n    %s\n    snippet: %.80s\n    content: %d chars\n", i, r.Title, r.URL, r.Snippet, len(r.Content))
	}
	if len(results) == 0 {
		t.Errorf("REAL search returned 0 results (DDG markup/status changed?)")
	}

	// --- fetch a real page ---
	raw2, _ := json.Marshal(map[string]any{"url": "https://example.com", "format": "text"})
	fres, ferr := m.fetch(context.Background(), raw2)
	if ferr != nil {
		t.Fatalf("REAL fetch failed: %v", ferr)
	}
	fd := fres.Data.(map[string]any)
	fmt.Printf("\n=== FETCH example.com ===\ntitle=%v len=%v\ncontent: %.160s\n", fd["title"], fd["length"], fd["content"])
}
