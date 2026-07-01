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

	"github.com/digitornai/digitorn/internal/embeddings"
	"github.com/digitornai/digitorn/internal/indexer"
)

// End-to-end : the SAME MySQL → RAG indexing + semantic query, run against
// EVERY vector backend (Qdrant, pgvector, Elasticsearch). Proves the whole
// pipeline is backend-agnostic — the right product comes back regardless of
// where the vectors live.
func TestRAG_BackendRotation_E2E(t *testing.T) {
	native := os.Getenv("DBACCESS_MYSQL_NATIVE")
	if native == "" {
		native = "root:root@tcp(localhost:3307)/ragtest"
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
	for _, s := range []string{
		"DROP TABLE IF EXISTS catalog_items",
		"CREATE TABLE catalog_items (id int PRIMARY KEY, name varchar(128), description text)",
		"INSERT INTO catalog_items VALUES " +
			"(1,'Aurora Wireless Headphones','Over-ear Bluetooth headphones with active noise cancellation and 30-hour battery for travel.')," +
			"(2,'TrailMaster Hiking Boots','Waterproof leather hiking boots with ankle support for rough terrain.')," +
			"(3,'ChefPro Blender','High-power 1200W kitchen blender for smoothies and crushing ice.')," +
			"(4,'PowerCore Battery Pack','20000mAh portable USB-C charger for phones and tablets.')",
	} {
		if _, err := raw.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	raw.Close()

	mgr := embeddings.NewManager("", embeddings.ModeONNX, false, nil)
	defer mgr.Close()

	backends := []struct {
		name    string
		backend map[string]any
	}{
		{"qdrant", map[string]any{"type": "qdrant", "url": envOrDefault("QDRANT_URL", "localhost:6334")}},
		{"pgvector", map[string]any{"type": "pgvector", "dsn": envOrDefault("DBACCESS_PG_DSN", "postgres://postgres:postgres@localhost:5433/postgres")}},
		{"elasticsearch", map[string]any{"type": "elasticsearch", "url": envOrDefault("ES_URL", "http://localhost:9200")}},
	}

	src := SourceConfig{
		Name: "items", Type: "mysql", KnowledgeBase: "rotcat",
		DSN:         "mysql://root:root@localhost:3307/ragtest",
		Query:       "SELECT id, name, description FROM catalog_items ORDER BY id",
		IDColumn:    "id",
		TextColumns: []string{"name", "description"},
	}

	for _, bk := range backends {
		t.Run(bk.name, func(t *testing.T) {
			cfg, _ := ParseConfig(map[string]any{
				"embedding_model": "minilm-l12",
				"backend":         bk.backend,
				"pipeline":        map[string]any{"retrieval": "semantic"},
			})
			be, err := newBackend(cfg)
			if err != nil {
				t.Skipf("backend %s unavailable: %v", bk.name, err)
			}
			defer be.Close()
			if _, err := be.ListKBs(context.Background()); err != nil {
				t.Skipf("backend %s unreachable: %v", bk.name, err)
			}
			_ = be.DeleteKB(context.Background(), "rotcat")

			eng := NewEngine(cfg, be, onnxEmbedder{mgr: mgr}, nil)
			svc := indexer.NewService(nil, 4)
			rep, err := svc.Sync(context.Background(), genericDBSpec(src, AutoIndex{}), ragSink{eng: eng})
			if err != nil {
				t.Fatalf("[%s] index: %v", bk.name, err)
			}
			if rep.Added != 4 {
				t.Fatalf("[%s] indexed %d, want 4", bk.name, rep.Added)
			}
			hits, err := eng.Query(context.Background(), "rotcat", "wireless noise cancelling headphones", 2)
			if err != nil {
				t.Fatalf("[%s] query: %v", bk.name, err)
			}
			if len(hits) == 0 || (hits[0].Source != "1" && !strings.Contains(strings.ToLower(hits[0].Text), "headphones")) {
				t.Fatalf("[%s] wrong top hit: %+v", bk.name, hits)
			}
			t.Logf("[%s] OK — top hit source=%s score=%.3f", bk.name, hits[0].Source, hits[0].Score)
			_ = be.DeleteKB(context.Background(), "rotcat")
		})
	}
}

func envOrDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
