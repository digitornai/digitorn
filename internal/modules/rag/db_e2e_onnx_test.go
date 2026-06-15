//go:build onnx

package rag

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/mbathepaul/digitorn/internal/embeddings"
	"github.com/mbathepaul/digitorn/internal/indexer"
)

// End-to-end : index a REAL remote MySQL product catalog through the shared
// dbaccess socle → chunk+embed with REAL minilm → store in REAL Qdrant → a
// natural-language query retrieves the right product. This is the "index a
// long MySQL database, then ask a question" case, proven live.
//
//	ONNXRUNTIME_LIB=bin/onnxruntime.dll go test -tags onnx ./internal/modules/rag/ \
//	  -run TestRAG_MySQLIndex_E2E -v
func TestRAG_MySQLIndex_E2E(t *testing.T) {
	native := os.Getenv("DBACCESS_MYSQL_NATIVE")
	if native == "" {
		native = "root:root@tcp(localhost:3307)/ragtest"
	}
	qurl := os.Getenv("QDRANT_URL")
	if qurl == "" {
		qurl = "localhost:6334"
	}

	raw, err := sql.Open("mysql", native)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if raw.PingContext(ctx) != nil {
		t.Skipf("no mysql at %s", native)
	}
	seed := []string{
		"DROP TABLE IF EXISTS products",
		"CREATE TABLE products (id int PRIMARY KEY, name varchar(128), description text, category varchar(40))",
		"INSERT INTO products VALUES " +
			"(1,'Aurora Wireless Headphones','Over-ear Bluetooth headphones with active noise cancellation and 30-hour battery, ideal for flights and travel.','audio')," +
			"(2,'TrailMaster Hiking Boots','Waterproof leather hiking boots with ankle support for rough mountain terrain.','footwear')," +
			"(3,'ChefPro Blender','High-power 1200W kitchen blender for smoothies, soups and crushing ice.','kitchen')," +
			"(4,'LumaDesk LED Lamp','Adjustable desk lamp with warm and cool light modes and USB charging.','home')," +
			"(5,'PowerCore Battery Pack','20000mAh portable charger with fast USB-C charging for phones and tablets.','electronics')," +
			"(6,'Zephyr Running Shoes','Lightweight breathable running shoes with cushioned soles for marathons.','footwear')," +
			"(7,'GardenGrow Tomato Seeds','Organic heirloom tomato seeds, easy to grow with a high yield.','garden')," +
			"(8,'CloudSleep Memory Pillow','Ergonomic memory foam pillow for neck support and better sleep.','home')",
	}
	for _, s := range seed {
		if _, err := raw.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	raw.Close()

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
	_ = be.DeleteKB(context.Background(), "catalog")

	eng := NewEngine(cfg, be, onnxEmbedder{mgr: mgr}, nil)

	src := SourceConfig{
		Name: "products", Type: "mysql", KnowledgeBase: "catalog",
		DSN:         "mysql://root:root@localhost:3307/ragtest",
		Query:       "SELECT id, name, description, category FROM products ORDER BY id",
		IDColumn:    "id",
		TextColumns: []string{"name", "description"},
	}
	svc := indexer.NewService(nil, 4)
	rep, err := svc.Sync(context.Background(), genericDBSpec(src, AutoIndex{}), ragSink{eng: eng})
	if err != nil {
		t.Fatalf("indexer sync: %v", err)
	}
	t.Logf("mysql→rag index: added=%d updated=%d deleted=%d", rep.Added, rep.Updated, rep.Deleted)
	if rep.Added != 8 {
		t.Fatalf("indexed %d products, want 8", rep.Added)
	}

	hits, err := eng.Query(context.Background(), "catalog", "wireless noise cancelling headphones for a long flight", 3)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("query returned no hits")
	}
	for i, h := range hits {
		txt := h.Text
		if len(txt) > 80 {
			txt = txt[:80]
		}
		t.Logf("  hit#%d source=%s score=%.3f %s", i+1, h.Source, h.Score, txt)
	}
	top := hits[0]
	if top.Source != "1" && !strings.Contains(strings.ToLower(top.Text), "headphones") {
		t.Fatalf("top hit is not the headphones product: source=%s text=%q", top.Source, top.Text)
	}

	_ = be.DeleteKB(context.Background(), "catalog")
}
