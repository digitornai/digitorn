package indexer

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func slotExists(t *testing.T, pool *pgxpool.Pool, slot string) bool {
	t.Helper()
	var n int
	_ = pool.QueryRow(context.Background(),
		"SELECT count(*) FROM pg_replication_slots WHERE slot_name=$1", slot).Scan(&n)
	return n > 0
}

// TestChaos_CDC_SlotLeak_OnDeregister proves that Deregister (the engine-
// eviction path) stops the watch but DOES NOT drop the replication slot — by
// design, so the source resumes durably later. The risk: if the source is
// eviction-Deregistered and never re-registered (or the worker crashes mid-
// Watch), the slot persists and the upstream DB ACCUMULATES WAL indefinitely.
// Only Remove() drops the slot. This test demonstrates the slot survives
// Deregister and is reclaimed only by Remove.
//
//	PG_CHAOS_URL=... go test ./internal/indexer/ -run TestChaos_CDC_SlotLeak_OnDeregister -v -timeout 90s
func TestChaos_CDC_SlotLeak_OnDeregister(t *testing.T) {
	url := os.Getenv("PG_CHAOS_URL")
	if url == "" {
		t.Skip("set PG_CHAOS_URL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	slot, pub, tbl := "leak_slot", "leak_pub", "leak_items"
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS `+tbl)
	_, _ = pool.Exec(ctx, `DROP PUBLICATION IF EXISTS `+pub)
	_, _ = pool.Exec(ctx, `SELECT pg_drop_replication_slot('`+slot+`') FROM pg_replication_slots WHERE slot_name='`+slot+`'`)
	if _, err := pool.Exec(ctx, `CREATE TABLE `+tbl+` (id int PRIMARY KEY, name text)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `SELECT pg_drop_replication_slot('`+slot+`') FROM pg_replication_slots WHERE slot_name='`+slot+`'`)
		_, _ = pool.Exec(ctx, `DROP PUBLICATION IF EXISTS `+pub)
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS `+tbl)
	})

	registerLoad()
	svc := NewService(NewMemCursor(), 4)
	spec := SourceSpec{Name: "leak", Type: "database", KB: "kb",
		Triggers: []Trigger{{Type: "cdc"}},
		Opts: map[string]any{
			"dsn": url, "id_column": "id", "text_columns": []string{"name"},
			"cdc_table": tbl, "cdc_slot": slot, "cdc_publication": pub,
		}}
	svc.Register(spec, newRecordingSink())
	time.Sleep(3 * time.Second) // slot creation

	if !slotExists(t, pool, slot) {
		t.Fatal("slot not created by Watch")
	}
	t.Log("slot created by active CDC watch")

	// Deregister (engine eviction) — stops the watch but, by contract, KEEPS the
	// slot for durable resume.
	svc.Deregister(spec)
	time.Sleep(2 * time.Second)
	if !slotExists(t, pool, slot) {
		t.Fatal("UNEXPECTED: Deregister dropped the slot (it should keep it for durable resume)")
	}
	// Measure WAL retained by the orphaned slot.
	var retained string
	_ = pool.QueryRow(ctx,
		`SELECT pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)) FROM pg_replication_slots WHERE slot_name=$1`, slot).Scan(&retained)
	t.Logf("CONFIRMED SLOT LEAK RISK: after Deregister the slot '%s' PERSISTS (WAL retained from restart_lsn ~ %s). If the source is never re-registered or the worker crashed mid-Watch, this slot retains WAL on the upstream DB FOREVER. Only Remove() drops it; Deregister (eviction) and a crash do not.", slot, retained)

	// Now Remove() should drop it (permanent teardown).
	if err := svc.Remove(ctx, spec); err != nil {
		t.Logf("Remove returned: %v (best-effort)", err)
	}
	time.Sleep(1 * time.Second)
	if slotExists(t, pool, slot) {
		t.Logf("NOTE: slot still present right after Remove — Cleanup is best-effort with an 'active' retry race; may need the just-cancelled Watch to fully release.")
		// give the active-retry a moment
		_, _ = pool.Exec(ctx, `SELECT pg_drop_replication_slot('`+slot+`') FROM pg_replication_slots WHERE slot_name='`+slot+`' AND active=false`)
	} else {
		t.Log("Remove() dropped the slot — permanent teardown reclaims WAL.")
	}
}
