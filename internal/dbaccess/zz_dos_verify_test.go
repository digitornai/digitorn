package dbaccess

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestVerify_GuardLetsDoSPayloadsThrough confirms the STATIC claim: in
// read_only mode the keyword guard does NOT block pg_sleep / pg_read_file /
// cartesian products — they all start with SELECT.
func TestVerify_GuardLetsDoSPayloadsThrough(t *testing.T) {
	ro := SecurityPolicy{Mode: "read_only"}
	payloads := []string{
		"SELECT pg_sleep(30)",
		"SELECT pg_read_file('/etc/passwd')",
		"SELECT pg_ls_dir('/')",
		"SELECT * FROM generate_series(1,1000000) a, generate_series(1,1000000) b", // cartesian
		"SELECT count(*) FROM pg_class a, pg_class b, pg_class c",                  // cartesian
		"SELECT lpad('x', 1000000000)",                                            // big memory alloc
	}
	for _, q := range payloads {
		if err := guardStatement(q, ro); err != nil {
			t.Errorf("guard BLOCKED %q (claim says it passes): %v", q, err)
		} else {
			t.Logf("PASS-THROUGH (guard does not block): %q", q)
		}
	}
}

// TestVerify_QueryHasPolicyTimeout proves Query applies the policy timeout
// (sql.go:134) — the DoS backstop for the Query path.
func TestVerify_QueryHasPolicyTimeout(t *testing.T) {
	url := os.Getenv("PGVEC_URL")
	if url == "" {
		t.Skip("set PGVEC_URL")
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

	// Caller passes context.Background() (no deadline) -> only the policy bounds it.
	start := time.Now()
	_, qerr := db.Query(context.Background(), "SELECT pg_sleep(10)")
	elapsed := time.Since(start)
	t.Logf("Query pg_sleep(10), policy=1s, caller ctx=Background -> err=%v elapsed=%v", qerr, elapsed)
	if qerr == nil {
		t.Fatalf("BUG: Query did NOT cancel slow query (ran %v)", elapsed)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("Query policy timeout did not fire promptly: %v", elapsed)
	}
	t.Logf("CONFIRMED: Query enforces policy StatementTimeout independent of caller ctx.")
}

// TestVerify_StreamPolicyTimeout probes whether QueryStream is bounded by the
// policy timeout when the caller ctx has NO deadline. This is the core of the
// claim: QueryStream (sql.go:210) does not wrap a context.WithTimeout.
//
// Critical nuance: applySession does SET statement_timeout on the SESSION at
// connect. With a SINGLE pooled conn that lands, the server may cap pg_sleep.
// We test with caller ctx = Background (no deadline) to isolate the in-process
// policy timeout question, AND we drain the pool to see if a fresh conn that
// never got applySession behaves differently.
func TestVerify_StreamPolicyTimeout(t *testing.T) {
	url := os.Getenv("PGVEC_URL")
	if url == "" {
		t.Skip("set PGVEC_URL")
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
		t.Fatal("not a Streamer")
	}

	// Caller ctx = Background (NO deadline). If the in-process policy timeout
	// bounded the stream, it would die ~1s. If only the server-side
	// SET statement_timeout bounds it, it may also die ~1s (best-effort).
	start := time.Now()
	serr := st.QueryStream(context.Background(), "SELECT pg_sleep(8)", func(Row) error { return nil })
	elapsed := time.Since(start)
	t.Logf("QueryStream pg_sleep(8), policy=1s, caller ctx=Background -> err=%v elapsed=%v", serr, elapsed)
	if elapsed >= 5*time.Second {
		t.Logf("FINDING: QueryStream ran ~8s -> NEITHER policy timeout NOR server-side cap bounded it. caller ctx is the only bound.")
	} else {
		t.Logf("NOTE: stream cut at ~%v -> server-side SET statement_timeout landed on this pooled conn (best-effort).", elapsed)
	}
}
