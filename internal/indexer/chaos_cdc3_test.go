package indexer

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestChaos_CDC_SlotResumesFromConfirmedFlush is the CONTROL proving the fix
// direction for the data-loss found in TestChaos_CDC_KillBeforeLSNFlush. It
// replicates the connector's setup but, on restart with an EMPTY cursor, starts
// replication at LSN(0) — which makes Postgres resume from the slot's own
// confirmed_flush_lsn instead of "now". If this delivers the downtime events,
// the bug fix is: drop the IdentifySystem-XLogPos override and start at 0 when
// the cursor is empty (the slot already tracks the position).
//
//	PG_CHAOS_URL=... go test ./internal/indexer/ -run TestChaos_CDC_SlotResumesFromConfirmedFlush -v
func TestChaos_CDC_SlotResumesFromConfirmedFlush(t *testing.T) {
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

	slot, pub, tbl := "chaos3_slot", "chaos3_pub", "chaos3_items"
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS `+tbl)
	_, _ = pool.Exec(ctx, `DROP PUBLICATION IF EXISTS `+pub)
	_, _ = pool.Exec(ctx, `SELECT pg_drop_replication_slot('`+slot+`') FROM pg_replication_slots WHERE slot_name='`+slot+`'`)
	if _, err := pool.Exec(ctx, `CREATE TABLE `+tbl+` (id int PRIMARY KEY, name text)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE PUBLICATION `+pub+` FOR TABLE `+tbl); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `SELECT pg_drop_replication_slot('`+slot+`') FROM pg_replication_slots WHERE slot_name='`+slot+`'`)
		_, _ = pool.Exec(ctx, `DROP PUBLICATION IF EXISTS `+pub)
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS `+tbl)
	})

	// Create the slot (this is what lifetime-1 would do).
	rc, err := pgconn.Connect(ctx, replicationDSN(url))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pglogrepl.CreateReplicationSlot(ctx, rc, slot, "pgoutput", pglogrepl.CreateReplicationSlotOptions{}); err != nil {
		t.Fatalf("create slot: %v", err)
	}
	rc.Close(ctx)

	// Insert rows AFTER slot creation, while NO consumer is reading (the slot
	// retains them). This is the "downtime" scenario.
	for i := 1; i <= 3; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO `+tbl+` (id,name) VALUES ($1,$2)`, i, fmt.Sprintf("r%d", i)); err != nil {
			t.Fatal(err)
		}
	}

	// Now connect a fresh consumer and START AT LSN 0 (the proposed fix). Read
	// for a few seconds and count how many of rows 1..3 arrive.
	conn, err := pgconn.Connect(ctx, replicationDSN(url))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	args := []string{"proto_version '1'", "publication_names '" + pub + "'"}
	if err := pglogrepl.StartReplication(ctx, conn, slot, pglogrepl.LSN(0), pglogrepl.StartReplicationOptions{PluginArgs: args}); err != nil {
		t.Fatalf("start replication at 0: %v", err)
	}

	relations := map[uint32]*pglogrepl.RelationMessage{}
	got := map[string]bool{}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && len(got) < 3 {
		rctx, cancel := context.WithTimeout(ctx, time.Second)
		raw, err := conn.ReceiveMessage(rctx)
		cancel()
		if err != nil {
			if pgconn.Timeout(err) {
				continue
			}
			break
		}
		cd, ok := raw.(*pgproto3.CopyData)
		if !ok || len(cd.Data) == 0 || cd.Data[0] != pglogrepl.XLogDataByteID {
			continue
		}
		xld, err := pglogrepl.ParseXLogData(cd.Data[1:])
		if err != nil {
			continue
		}
		m, err := pglogrepl.Parse(xld.WALData)
		if err != nil {
			continue
		}
		switch v := m.(type) {
		case *pglogrepl.RelationMessage:
			relations[v.RelationID] = v
		case *pglogrepl.InsertMessage:
			if rel := relations[v.RelationID]; rel != nil {
				row := tupleRow(rel, v.Tuple)
				got[dbStr(row["id"])] = true
			}
		}
	}

	t.Logf("StartReplication(LSN=0) delivered rows: %v", got)
	if len(got) == 3 {
		t.Log("FIX CONFIRMED: starting at LSN(0) resumes from the slot's confirmed_flush_lsn and delivers all retained downtime rows. The IdentifySystem-XLogPos override in connector_db.go:203-207 is the root cause of the kill-before-flush data loss.")
	} else {
		t.Errorf("StartReplication(LSN=0) delivered only %d/3 retained rows: %v", len(got), got)
	}
}
