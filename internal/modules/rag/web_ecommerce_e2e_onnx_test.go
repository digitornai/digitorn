//go:build onnx

package rag

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/embeddings"
	"github.com/mbathepaul/digitorn/internal/indexer"
)

// End-to-end : crawl a realistic e-commerce catalog (an Amazon-style product
// site served locally) via the web connector → embed with REAL minilm → store
// in REAL Qdrant → a shopper's natural-language query retrieves the right
// product page. This is the "index a whole product site, then ask for a
// product" case. (Real amazon.com is bot-blocked + JS-rendered, so the catalog
// is served locally and crawled by the same connector.)
func TestRAG_EcommerceWebIndex_E2E(t *testing.T) {
	qurl := os.Getenv("QDRANT_URL")
	if qurl == "" {
		qurl = "localhost:6334"
	}

	products := []struct {
		ID         int
		Name, Desc string
	}{
		{1, "Aurora Wireless Headphones", "Over-ear Bluetooth headphones with active noise cancellation and a 30-hour battery, ideal for flights and travel."},
		{2, "TrailMaster Hiking Boots", "Waterproof leather hiking boots with ankle support for rough mountain terrain."},
		{3, "ChefPro Blender", "High-power 1200W kitchen blender for smoothies, soups and crushing ice."},
		{4, "PowerCore Battery Pack", "20000mAh portable charger with fast USB-C charging for phones and tablets."},
		{5, "Zephyr Running Shoes", "Lightweight breathable running shoes with cushioned soles for marathons."},
		{6, "CloudSleep Memory Pillow", "Ergonomic memory foam pillow for neck support and better sleep."},
		{7, "GardenGrow Tomato Seeds", "Organic heirloom tomato seeds, easy to grow with a high yield."},
		{8, "LumaDesk LED Lamp", "Adjustable desk lamp with warm and cool light modes and USB charging."},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		var b strings.Builder
		b.WriteString("<html><head><title>Shop</title></head><body><h1>Catalog</h1><ul>")
		for _, p := range products {
			fmt.Fprintf(&b, `<li><a href="/product/%d">%s</a></li>`, p.ID, p.Name)
		}
		b.WriteString("</ul></body></html>")
		_, _ = w.Write([]byte(b.String()))
	})
	mux.HandleFunc("/product/", func(w http.ResponseWriter, r *http.Request) {
		var id int
		if _, err := fmt.Sscanf(r.URL.Path, "/product/%d", &id); err != nil || id < 1 || id > len(products) {
			http.NotFound(w, r)
			return
		}
		p := products[id-1]
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><head><title>%s</title></head><body><h1>%s</h1><p class="desc">%s</p></body></html>`, p.Name, p.Name, p.Desc)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

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
	_ = be.DeleteKB(context.Background(), "shop")
	eng := NewEngine(cfg, be, onnxEmbedder{mgr: mgr}, nil)

	off, on := false, true
	src := SourceConfig{
		Name: "shop", Type: "web", KnowledgeBase: "shop", URL: srv.URL,
		MaxPages: 30, Sitemap: &off, RespectRobots: &off, AllowPrivate: &on, // local test server is loopback
	}
	svc := indexer.NewService(nil, 4)
	rep, err := svc.Sync(context.Background(), webSpec(src, AutoIndex{}), ragSink{eng: eng})
	if err != nil {
		t.Fatalf("crawl+index: %v", err)
	}
	t.Logf("ecommerce crawl+index: added=%d", rep.Added)
	if rep.Added < 8 {
		t.Fatalf("indexed %d pages, want >= 8 product pages", rep.Added)
	}

	hits, err := eng.Query(context.Background(), "shop", "wireless headphones with noise cancellation for flights", 3)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	top := hits[0]
	t.Logf("ecommerce TOP HIT source=%s score=%.3f", top.Source, top.Score)
	if !strings.Contains(top.Source, "/product/1") && !strings.Contains(strings.ToLower(top.Text), "headphones") {
		t.Fatalf("top hit not the headphones page: source=%s text=%q", top.Source, top.Text)
	}
	_ = be.DeleteKB(context.Background(), "shop")
}
