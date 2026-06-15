package dbaccess

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestChaos_ConnectionDrop_MidQuery kills the Postgres container while a query
// is in flight and verifies the error surfaces cleanly (no hang past ctx, no
// panic) and that a subsequent query after the DB recovers either reconnects
// (database/sql pool self-heals) or errors cleanly.
//
//	PG_CHAOS_URL=postgres://postgres:postgres@localhost:5499/postgres \
//	PG_CHAOS_CONTAINER=pg-chaos \
//	  go test ./internal/dbaccess/ -run TestChaos_ConnectionDrop_MidQuery -v -timeout 120s
func TestChaos_ConnectionDrop_MidQuery(t *testing.T) {
	url := os.Getenv("PG_CHAOS_URL")
	cname := os.Getenv("PG_CHAOS_CONTAINER")
	if url == "" || cname == "" {
		t.Skip("set PG_CHAOS_URL and PG_CHAOS_CONTAINER")
	}
	ctx := context.Background()
	db, err := Open(ctx, ConnConfig{
		Kind: "postgres", DSN: url,
		Security: SecurityPolicy{Mode: "read_only", StatementTimeout: 30 * time.Second},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// Confirm baseline query works.
	if _, err := db.Query(ctx, "SELECT 1"); err != nil {
		t.Fatalf("baseline query failed: %v", err)
	}

	// Launch a slow query, then kill the container while it runs.
	type qres struct {
		err     error
		elapsed time.Duration
	}
	resCh := make(chan qres, 1)
	go func() {
		start := time.Now()
		_, e := db.Query(ctx, "SELECT pg_sleep(20)")
		resCh <- qres{e, time.Since(start)}
	}()

	time.Sleep(2 * time.Second) // let the slow query establish
	t.Log("stopping container mid-query")
	if out, err := exec.Command("docker", "stop", "-t", "1", cname).CombinedOutput(); err != nil {
		t.Fatalf("docker stop: %v: %s", err, out)
	}

	select {
	case r := <-resCh:
		t.Logf("in-flight query returned after container kill: err=%v elapsed=%v", r.err, r.elapsed)
		if r.err == nil {
			t.Errorf("BUG: query returned nil error despite the DB being killed mid-flight")
		}
		if r.elapsed > 25*time.Second {
			t.Errorf("query hung past a reasonable bound (%v) after connection drop", r.elapsed)
		}
	case <-time.After(35 * time.Second):
		t.Errorf("BUG: in-flight query HUNG after the DB was killed (no error within 35s)")
	}

	// Restart the container and confirm the pool self-heals on the next query.
	t.Log("restarting container; checking pool self-heal")
	if out, err := exec.Command("docker", "start", cname).CombinedOutput(); err != nil {
		t.Fatalf("docker start: %v: %s", err, out)
	}
	// Wait for readiness.
	ready := false
	for i := 0; i < 40; i++ {
		cc, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, qe := db.Query(cc, "SELECT 1")
		cancel()
		if qe == nil {
			ready = true
			t.Logf("pool self-healed after %d attempts; SELECT 1 works again", i+1)
			break
		}
		time.Sleep(1 * time.Second)
	}
	if !ready {
		t.Errorf("pool did NOT self-heal: SELECT 1 still failing after container restart")
	}
}

// TestChaos_NetworkRefuse_Open proves Open fails cleanly (bounded by the 10s
// ping timeout) when the target host refuses connections — no indefinite hang.
func TestChaos_NetworkRefuse_Open(t *testing.T) {
	ctx := context.Background()
	// Port 1 is reserved and nothing listens — connection refused immediately.
	start := time.Now()
	_, err := Open(ctx, ConnConfig{
		Kind: "postgres",
		DSN:  "postgres://postgres:postgres@127.0.0.1:1/postgres?sslmode=disable",
	})
	elapsed := time.Since(start)
	t.Logf("Open against refused port: err=%v elapsed=%v", err, elapsed)
	if err == nil {
		t.Fatal("BUG: Open succeeded against a refused port")
	}
	if elapsed > 12*time.Second {
		t.Errorf("Open hung past the 10s ping bound: %v", elapsed)
	}
	t.Log("CONFIRMED: Open fails cleanly + bounded on connection refused.")
}
