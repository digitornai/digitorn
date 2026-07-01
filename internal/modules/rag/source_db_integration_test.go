package rag

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/digitornai/digitorn/internal/indexer"
)

// Database source through the indexation service : SQL query → rows →
// documents → chunk+embed+store, incremental (insert/update/delete), against
// a real Postgres. Proves the db connector + service diff + ragSink.
func TestDatabaseSource_IncrementalSync(t *testing.T) {
	url := os.Getenv("PGVECTOR_URL")
	if url == "" {
		t.Skip("set PGVECTOR_URL to run the database-source test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS articles`)
	if _, err := pool.Exec(ctx, `CREATE TABLE articles (id int PRIMARY KEY, title text, body text, team text)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO articles (id,title,body,team) VALUES
		(1,'Deployment guide','how to deploy the application to the production server','ops'),
		(2,'Cake recipe','a chocolate cake with butter and sugar','kitchen')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cfg, _ := ParseConfig(map[string]any{
		"backend":  map[string]any{"type": "pgvector", "dsn": url},
		"pipeline": map[string]any{"retrieval": "semantic"},
	})
	be, err := newBackend(cfg)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	defer be.Close()
	_ = be.DeleteKB(ctx, "db")
	eng := NewEngine(cfg, be, fakeEmbedder{dim: 64}, nil)

	svc := indexer.NewService(nil, 2)
	spec := dbSpec(SourceConfig{
		Type: "database", DSN: url, KnowledgeBase: "db",
		Query: "SELECT id, title, body, team FROM articles ORDER BY id",
		IDColumn: "id", TextColumns: []string{"title", "body"},
	}, AutoIndex{})
	sink := ragSink{eng: eng}

	rep, err := svc.Sync(ctx, spec, sink)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if rep.Added != 2 {
		t.Fatalf("first sync Added=%d, want 2", rep.Added)
	}

	hits, err := eng.Query(ctx, "db", "deploy the application to the server", 3)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(hits) == 0 || hits[0].Source != "1" {
		t.Fatalf("top hit = %+v, want row id 1", hits)
	}
	if hits[0].Meta == nil || hits[0].Meta["team"] != "ops" {
		t.Errorf("row column metadata not synced as filterable meta: %+v", hits[0].Meta)
	}

	if rep, _ = svc.Sync(ctx, spec, sink); rep.Added+rep.Updated+rep.Deleted != 0 {
		t.Errorf("idempotent re-sync changed: %+v", rep)
	}

	if _, err := pool.Exec(ctx, `UPDATE articles SET body='new deployment instructions for kubernetes clusters' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if rep, _ = svc.Sync(ctx, spec, sink); rep.Updated != 1 {
		t.Errorf("after row update, Updated=%d want 1", rep.Updated)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM articles WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	if rep, _ = svc.Sync(ctx, spec, sink); rep.Deleted != 1 {
		t.Errorf("after row delete, Deleted=%d want 1", rep.Deleted)
	}
	if n, _ := be.CountKB(ctx, "db"); n != 1 {
		t.Errorf("kb count=%d want 1 after deleting row 2", n)
	}

	_ = be.DeleteKB(ctx, "db")
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS articles`)
}
