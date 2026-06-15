package indexer

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func (s *fakeSink) text(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upserts[id].Text
}
func (s *fakeSink) wasDeleted(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.deletes {
		if d == id {
			return true
		}
	}
	return false
}

func waitFor(cond func() bool, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return cond()
}

// Live CDC : stream a table's WAL and react to insert/update/delete in real
// time. Needs Postgres with wal_level=logical.
//
//	docker run -d -e POSTGRES_PASSWORD=postgres -p 5434:5432 pgvector/pgvector:pg16 \
//	  -c wal_level=logical -c max_wal_senders=4 -c max_replication_slots=4
//	PGLOGICAL_URL=postgres://postgres:postgres@localhost:5434/postgres \
//	  go test ./internal/indexer/ -run TestDBConnector_CDC_Live -v
func TestDBConnector_CDC_Live(t *testing.T) {
	url := os.Getenv("PGLOGICAL_URL")
	if url == "" {
		t.Skip("set PGLOGICAL_URL (wal_level=logical) to run the CDC test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS cdc_items`)
	_, _ = pool.Exec(ctx, `DROP PUBLICATION IF EXISTS cdc_test_pub`)
	_, _ = pool.Exec(ctx, `SELECT pg_drop_replication_slot('cdc_test_slot') FROM pg_replication_slots WHERE slot_name='cdc_test_slot'`)
	if _, err := pool.Exec(ctx, `CREATE TABLE cdc_items (id int PRIMARY KEY, name text)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	conn := &dbConnector{}
	spec := SourceSpec{Name: "cdc", Type: "database", KB: "kb", Opts: map[string]any{
		"dsn": url, "id_column": "id", "text_columns": []string{"name"},
		"cdc_table": "cdc_items", "cdc_slot": "cdc_test_slot", "cdc_publication": "cdc_test_pub",
	}}
	sink := newFakeSink()
	wctx, cancel := context.WithCancel(ctx)
	go func() { _ = conn.Watch(wctx, spec, sink, NewMemCursor()) }()

	time.Sleep(2 * time.Second) // let replication start

	if _, err := pool.Exec(ctx, `INSERT INTO cdc_items (id,name) VALUES (1,'alpha widget')`); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool { return sink.text("1") != "" }, 10*time.Second) {
		cancel()
		t.Fatal("CDC: insert not streamed to sink")
	}
	t.Logf("CDC insert received: %q", sink.text("1"))

	if _, err := pool.Exec(ctx, `UPDATE cdc_items SET name='alpha widget v2' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool { return strings.Contains(sink.text("1"), "v2") }, 10*time.Second) {
		cancel()
		t.Fatalf("CDC: update not streamed (text=%q)", sink.text("1"))
	}
	t.Logf("CDC update received: %q", sink.text("1"))

	if _, err := pool.Exec(ctx, `DELETE FROM cdc_items WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool { return sink.wasDeleted("1") }, 10*time.Second) {
		cancel()
		t.Fatal("CDC: delete not streamed to sink")
	}
	t.Log("CDC delete received")

	cancel()
	time.Sleep(500 * time.Millisecond)
	_, _ = pool.Exec(ctx, `SELECT pg_drop_replication_slot('cdc_test_slot') FROM pg_replication_slots WHERE slot_name='cdc_test_slot'`)
	_, _ = pool.Exec(ctx, `DROP PUBLICATION IF EXISTS cdc_test_pub`)
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS cdc_items`)
}
