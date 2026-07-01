//go:build onnx

package rag

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/digitornai/digitorn/internal/embeddings"
	"github.com/digitornai/digitorn/internal/indexer"
)

// End-to-end : index a REAL MongoDB collection through the shared dbaccess
// socle (query = a JSON find spec) → embed with REAL minilm → store in REAL
// Qdrant → a natural-language query retrieves the right document. Proves the
// NoSQL → RAG path on the same machinery as SQL.
func TestRAG_MongoIndex_E2E(t *testing.T) {
	dsn := os.Getenv("DBACCESS_MONGO_DSN")
	if dsn == "" {
		dsn = "mongodb://localhost:27018/ragtest"
	}
	qurl := os.Getenv("QDRANT_URL")
	if qurl == "" {
		qurl = "localhost:6334"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cli, err := mongo.Connect(options.Client().ApplyURI(dsn))
	if err != nil || cli.Ping(ctx, nil) != nil {
		t.Skipf("no mongo at %s", dsn)
	}
	coll := cli.Database("ragtest").Collection("products")
	_ = coll.Drop(ctx)
	_, err = coll.InsertMany(ctx, []any{
		bson.M{"_id": 1, "name": "Aurora Wireless Headphones", "description": "Over-ear Bluetooth headphones with active noise cancellation and 30-hour battery, ideal for flights and travel.", "category": "audio"},
		bson.M{"_id": 2, "name": "TrailMaster Hiking Boots", "description": "Waterproof leather hiking boots with ankle support for rough mountain terrain.", "category": "footwear"},
		bson.M{"_id": 3, "name": "ChefPro Blender", "description": "High-power 1200W kitchen blender for smoothies, soups and crushing ice.", "category": "kitchen"},
		bson.M{"_id": 4, "name": "PowerCore Battery Pack", "description": "20000mAh portable charger with fast USB-C charging for phones and tablets.", "category": "electronics"},
		bson.M{"_id": 5, "name": "CloudSleep Memory Pillow", "description": "Ergonomic memory foam pillow for neck support and better sleep.", "category": "home"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = cli.Disconnect(ctx)

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
	_ = be.DeleteKB(context.Background(), "mongocat")
	eng := NewEngine(cfg, be, onnxEmbedder{mgr: mgr}, nil)

	src := SourceConfig{
		Name: "products", Type: "mongodb", KnowledgeBase: "mongocat",
		DSN:         dsn,
		Query:       `{"collection":"products","sort":{"_id":1}}`,
		IDColumn:    "_id",
		TextColumns: []string{"name", "description"},
	}
	svc := indexer.NewService(nil, 4)
	rep, err := svc.Sync(context.Background(), genericDBSpec(src, AutoIndex{}), ragSink{eng: eng})
	if err != nil {
		t.Fatalf("indexer sync: %v", err)
	}
	t.Logf("mongo→rag index: added=%d", rep.Added)
	if rep.Added != 5 {
		t.Fatalf("indexed %d docs, want 5", rep.Added)
	}

	hits, err := eng.Query(context.Background(), "mongocat", "noise cancelling headphones for travel", 3)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	top := hits[0]
	t.Logf("mongo TOP HIT source=%s score=%.3f", top.Source, top.Score)
	if top.Source != "1" && !strings.Contains(strings.ToLower(top.Text), "headphones") {
		t.Fatalf("top hit not the headphones doc: source=%s text=%q", top.Source, top.Text)
	}
	_ = be.DeleteKB(context.Background(), "mongocat")
}
