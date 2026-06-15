package indexer

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestChaos_CDC_KillBeforeLSNFlush probes the window BEFORE the first standby/
// LSN flush. The connector only persists the LSN every ~10s (nextStandby) or on
// a reply-requested keepalive. If a worker is killed within that window, the
// cursor holds NO LSN. On restart with an empty cursor the connector calls
// IdentifySystem -> current XLogPos and StartReplication from there.
//
// CRITICAL QUESTION: with the replication SLOT already created (lifetime 1), does
// a restart-from-current-XLogPos LOSE the events between the slot's confirmed
// flush LSN and "now"? Postgres replays from the slot's restart_lsn, but the
// connector tells StartReplication to start at current XLogPos when its cursor is
// empty — overriding the slot's own position. This test measures actual loss.
//
//	PG_CHAOS_URL=... go test ./internal/indexer/ -run TestChaos_CDC_KillBeforeLSNFlush -v
func TestChaos_CDC_KillBeforeLSNFlush(t *testing.T) {
	url := os.Getenv("PG_CHAOS_URL")
	if url == "" {
		t.Skip("set PG_CHAOS_URL (wal_level=logical) to run")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS chaos2_items`)
	_, _ = pool.Exec(ctx, `DROP PUBLICATION IF EXISTS chaos2_pub`)
	_, _ = pool.Exec(ctx, `SELECT pg_drop_replication_slot('chaos2_slot') FROM pg_replication_slots WHERE slot_name='chaos2_slot'`)
	if _, err := pool.Exec(ctx, `CREATE TABLE chaos2_items (id int PRIMARY KEY, name text)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `SELECT pg_drop_replication_slot('chaos2_slot') FROM pg_replication_slots WHERE slot_name='chaos2_slot'`)
		_, _ = pool.Exec(ctx, `DROP PUBLICATION IF EXISTS chaos2_pub`)
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS chaos2_items`)
	})

	conn := &dbConnector{}
	spec := SourceSpec{Name: "chaos2", Type: "database", KB: "kb", Opts: map[string]any{
		"dsn": url, "id_column": "id", "text_columns": []string{"name"},
		"cdc_table": "chaos2_items", "cdc_slot": "chaos2_slot", "cdc_publication": "chaos2_pub",
	}}
	cur := NewMemCursor()
	sink := newRecordingSink()

	// Lifetime 1: start the watcher (creates the slot), insert rows 1..3, and
	// kill it FAST (< 10s) so no LSN flush ever happens.
	w1ctx, cancel1 := context.WithCancel(ctx)
	go func() { _ = conn.Watch(w1ctx, spec, sink, cur) }()
	time.Sleep(2 * time.Second) // slot create + replication start

	for i := 1; i <= 3; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO chaos2_items (id,name) VALUES ($1,$2)`, i, fmt.Sprintf("row-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	// Wait only until streamed (well under the 10s flush interval), then KILL.
	if !waitUntil(func() bool { return sink.seen("1") && sink.seen("2") && sink.seen("3") }, 8*time.Second) {
		cancel1()
		t.Fatalf("lifetime1: rows 1..3 not streamed before kill window")
	}
	cancel1()
	time.Sleep(500 * time.Millisecond)

	lsnKey := stateKey(spec) + ":lsn"
	saved, _ := cur.Load(lsnKey)
	t.Logf("cursor LSN after fast kill = %q (empty => restart uses IdentifySystem XLogPos)", string(saved))

	// Downtime inserts 4..5 while no watcher runs.
	for i := 4; i <= 5; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO chaos2_items (id,name) VALUES ($1,$2)`, i, fmt.Sprintf("row-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("downtime: inserted rows 4..5 (no watcher)")

	// Lifetime 2: restart with the (possibly empty) cursor.
	w2ctx, cancel2 := context.WithCancel(ctx)
	go func() { _ = conn.Watch(w2ctx, spec, sink, cur) }()
	defer cancel2()

	got45 := waitUntil(func() bool { return sink.seen("4") && sink.seen("5") }, 12*time.Second)
	t.Logf("after restart: row4 seen=%v row5 seen=%v", sink.seen("4"), sink.seen("5"))

	if len(saved) == 0 && !got45 {
		t.Logf("DATA-LOSS CONFIRMED: with an empty cursor (kill before 10s flush), restart from IdentifySystem XLogPos SKIPS the downtime events 4..5 even though the slot retained them. The connector overrides the slot position with 'now'.")
	}
	if !got45 {
		// Make the failure loud but classify it as the documented gap.
		t.Errorf("downtime events 4..5 lost after fast-kill restart (got45=%v, savedLSN=%q)", got45, string(saved))
	}
}
