package dbaccess

import (
	"context"
	"os"
	"testing"
	"time"
)

// chaosPGURL is the dedicated chaos Postgres (kill-able). Falls back to the
// shared pgvec-rag for the read-only slow-query test (non-destructive).
func chaosPGURL() string {
	if u := os.Getenv("PG_CHAOS_URL"); u != "" {
		return u
	}
	return os.Getenv("PGVEC_URL")
}

// TestChaos_StatementTimeout_SlowQuery proves the Query path enforces the
// policy StatementTimeout: a pg_sleep longer than the timeout is cancelled.
// This is the DoS backstop for read_only (which lets pg_sleep/cartesian joins
// through the keyword guard).
//
//	PGVEC_URL=postgres://postgres:postgres@localhost:5433/postgres \
//	  go test ./internal/dbaccess/ -run TestChaos_StatementTimeout_SlowQuery -v
func TestChaos_StatementTimeout_SlowQuery(t *testing.T) {
	url := chaosPGURL()
	if url == "" {
		t.Skip("set PGVEC_URL or PG_CHAOS_URL")
	}
	ctx := context.Background()
	db, err := Open(ctx, ConnConfig{
		Kind: "postgres", DSN: url,
		Security: SecurityPolicy{Mode: "read_only", StatementTimeout: 1500 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// pg_sleep(5) under a 1.5s policy timeout MUST be cancelled.
	start := time.Now()
	_, qerr := db.Query(ctx, "SELECT pg_sleep(5)")
	elapsed := time.Since(start)
	t.Logf("pg_sleep(5) under 1.5s policy timeout: err=%v elapsed=%v", qerr, elapsed)

	if qerr == nil {
		t.Fatalf("BUG: slow query was NOT cancelled (ran %v)", elapsed)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("timeout did not fire promptly: elapsed=%v (want ~1.5s)", elapsed)
	}
	t.Logf("CONFIRMED: Query path enforces policy StatementTimeout (cancelled at %v).", elapsed)
}

// streamSink counts rows for the QueryStream timeout probe.
// TestChaos_StatementTimeout_StreamIgnoresPolicy proves QueryStream does NOT
// apply the policy timeout — only the caller's ctx bounds it. A long-ctx stream
// over a slow/huge query runs unbounded by the policy.
func TestChaos_StatementTimeout_StreamIgnoresPolicy(t *testing.T) {
	url := chaosPGURL()
	if url == "" {
		t.Skip("set PGVEC_URL or PG_CHAOS_URL")
	}
	ctx := context.Background()
	db, err := Open(ctx, ConnConfig{
		Kind: "postgres", DSN: url,
		Security: SecurityPolicy{Mode: "read_only", StatementTimeout: 1 * time.Second},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	st, ok := db.(Streamer)
	if !ok {
		t.Skip("not a Streamer")
	}

	// A 3s sleep under a 1s POLICY timeout, but a generous CALLER ctx (10s).
	// If the policy bound the stream, this would error ~1s. If it doesn't, it
	// runs the full 3s (server SET statement_timeout may or may not cap it,
	// depending on whether applySession landed on this pooled conn).
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := time.Now()
	serr := st.QueryStream(cctx, "SELECT pg_sleep(3)", func(Row) error { return nil })
	elapsed := time.Since(start)
	t.Logf("QueryStream pg_sleep(3): policy=1s callerCtx=10s -> err=%v elapsed=%v", serr, elapsed)

	if elapsed >= 2500*time.Millisecond {
		t.Logf("CONFIRMED: QueryStream is NOT bounded by the policy StatementTimeout (1s) — ran the full ~3s. Only the caller ctx (10s here; 10min in the indexer) bounds a stream. A stuck/huge stream stalls a worker slot for up to the caller ctx.")
	} else if elapsed < 1500*time.Millisecond {
		t.Logf("NOTE: stream was cut at ~%v — the server-side SET statement_timeout likely DID land on this pooled conn (best-effort, pool-dependent per sql.go applySession). Re-run may vary.", elapsed)
	}
}
