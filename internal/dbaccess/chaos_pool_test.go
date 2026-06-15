package dbaccess

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestChaos_SessionTimeout_PoolDependent probes whether the session-level
// SET statement_timeout (applied ONCE on a single pooled conn in applySession)
// actually covers ALL 8 pooled physical connections. It fires N concurrent slow
// streams (forcing the pool to open multiple physical conns) and measures
// whether any stream ESCAPES the server-side 1s cap because it ran on a conn
// that never received the SET.
//
// If the session guarantee were robust, EVERY stream would be cut at ~1s by the
// server. If it is pool-dependent (the documented risk), some streams running on
// un-SET conns would run the full sleep.
//
//	PGVEC_URL=... go test ./internal/dbaccess/ -run TestChaos_SessionTimeout_PoolDependent -v
func TestChaos_SessionTimeout_PoolDependent(t *testing.T) {
	url := chaosPGURL()
	if url == "" {
		t.Skip("set PGVEC_URL or PG_CHAOS_URL")
	}
	ctx := context.Background()
	// IMPORTANT: NO policy StatementTimeout and a long caller ctx, so the ONLY
	// thing that can cut the query is the server-side SET from applySession.
	// We set a policy timeout so applySession issues the SET, but use the stream
	// path (which does NOT apply the policy ctx) with a long caller ctx.
	db, err := Open(ctx, ConnConfig{
		Kind: "postgres", DSN: url,
		Security: SecurityPolicy{Mode: "read_only", StatementTimeout: 1 * time.Second},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	st := db.(Streamer)

	const n = 8 // == SetMaxOpenConns(8); forces the pool to spread conns
	var wg sync.WaitGroup
	durs := make([]time.Duration, n)
	errs := make([]error, n)
	cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start := time.Now()
			errs[i] = st.QueryStream(cctx, "SELECT pg_sleep(4)", func(Row) error { return nil })
			durs[i] = time.Since(start)
		}(i)
	}
	wg.Wait()

	var escaped, capped int
	for i := 0; i < n; i++ {
		if durs[i] >= 3500*time.Millisecond {
			escaped++
		} else {
			capped++
		}
		t.Logf("stream[%d]: elapsed=%v err=%v", i, durs[i], errs[i])
	}
	t.Logf("RESULT: %d/%d streams CAPPED by server (~1s), %d/%d ESCAPED to full ~4s", capped, n, escaped, n)
	if escaped > 0 {
		t.Logf("CONFIRMED RISK: %d concurrent streams ran the FULL ~4s, escaping the session statement_timeout — those ran on pooled physical conns that never received the SET. The session policy is pool-dependent best-effort; QueryStream has no per-query ctx timeout. A long-ctx (10min indexer) stream on an un-SET conn is effectively unbounded.", escaped)
	} else {
		t.Logf("NOTE: all streams were capped this run. The pool may have happened to apply SET broadly, or pgx applies it per-conn. Re-run; this is timing/pool dependent.")
	}
}
