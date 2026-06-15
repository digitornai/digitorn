package dbaccess

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func verifyPGURL() string {
	if u := os.Getenv("PGVEC_URL"); u != "" {
		return u
	}
	return os.Getenv("PG_CHAOS_URL")
}

// Verify_LRU_EvictionUseAfterClose — independent reproduction (not the
// claim's own test). max=1, two apps, app B's open evicts app A's in-use conn.
func TestVerify_LRU_EvictionUseAfterClose(t *testing.T) {
	url := verifyPGURL()
	if url == "" {
		t.Skip("set PGVEC_URL")
	}
	ctx := context.Background()
	mgr := NewManager(1, 30*time.Minute)
	defer mgr.Shutdown()
	cfg := ConnConfig{Name: "prod", Kind: "postgres", DSN: url, Security: SecurityPolicy{Mode: "read_only"}}

	dbA, err := mgr.Named(ctx, "appA", cfg)
	if err != nil {
		t.Fatalf("appA open: %v", err)
	}
	if _, err := dbA.Query(ctx, "SELECT 1"); err != nil {
		t.Fatalf("appA baseline: %v", err)
	}
	t.Logf("appA pool size before appB open: %d", len(mgr.conns))

	if _, err := mgr.Named(ctx, "appB", cfg); err != nil {
		t.Fatalf("appB open: %v", err)
	}
	t.Logf("pool size after appB open: %d (max=1)", len(mgr.conns))

	_, qerr := dbA.Query(ctx, "SELECT 1")
	t.Logf("appA query AFTER appB opened: err=%v", qerr)
	if qerr == nil {
		t.Fatalf("NO REPRO: appA query succeeded after appB open")
	}
	if !strings.Contains(strings.ToLower(qerr.Error()), "closed") {
		t.Fatalf("REPRO but unexpected error (not 'closed'): %v", qerr)
	}
	t.Logf("REPRODUCED: appA's live conn use-after-close: %v", qerr)
}

// Verify_TTL_EvictionUseAfterClose — the TTL branch the claim also mentions.
// A tiny TTL makes app A's conn idle-expire; app B's open triggers
// evictLocked which closes A while A still holds the handle.
func TestVerify_TTL_EvictionUseAfterClose(t *testing.T) {
	url := verifyPGURL()
	if url == "" {
		t.Skip("set PGVEC_URL")
	}
	ctx := context.Background()
	mgr := NewManager(256, 50*time.Millisecond) // big bound, tiny TTL
	defer mgr.Shutdown()
	cfg := ConnConfig{Name: "prod", Kind: "postgres", DSN: url, Security: SecurityPolicy{Mode: "read_only"}}

	dbA, err := mgr.Named(ctx, "appA", cfg)
	if err != nil {
		t.Fatalf("appA open: %v", err)
	}
	// Let A's usedAt age past TTL without touching it.
	time.Sleep(120 * time.Millisecond)

	// App B's open calls store()->evictLocked(now): A is now idle > TTL -> closed.
	if _, err := mgr.Named(ctx, "appB", cfg); err != nil {
		t.Fatalf("appB open: %v", err)
	}
	t.Logf("pool size after appB open (A should be TTL-evicted): %d", len(mgr.conns))

	_, qerr := dbA.Query(ctx, "SELECT 1")
	t.Logf("appA query AFTER TTL eviction: err=%v", qerr)
	if qerr == nil {
		t.Fatalf("NO REPRO via TTL: appA query succeeded")
	}
	t.Logf("REPRODUCED via TTL: %v", qerr)
}

// Verify_Refcount_Absent — sanity: there is no in-flight guard. We Close A
// directly then query; confirms Query on a closed sqlDB returns the closed
// error (the mechanism the eviction exploits).
func TestVerify_ClosedHandle_QueryFails(t *testing.T) {
	url := verifyPGURL()
	if url == "" {
		t.Skip("set PGVEC_URL")
	}
	ctx := context.Background()
	mgr := NewManager(256, 30*time.Minute)
	defer mgr.Shutdown()
	cfg := ConnConfig{Name: "x", Kind: "postgres", DSN: url, Security: SecurityPolicy{Mode: "read_only"}}
	db, err := mgr.Named(ctx, "app", cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = db.Close()
	_, qerr := db.Query(ctx, "SELECT 1")
	t.Logf("query on Closed handle: err=%v", qerr)
	if qerr == nil {
		t.Fatalf("expected error querying closed handle")
	}
}
