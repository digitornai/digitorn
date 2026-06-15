package dbaccess

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestChaos_Manager_EvictionUseAfterClose proves that the Manager's LRU
// eviction closes a pooled DB that another goroutine still holds and is about
// to use. With max=1, opening app B's connection evicts app A's — and A's
// subsequently-issued query fails with "database is closed". No refcount/lease
// protects an in-use pooled connection.
//
//	PGVEC_URL=... go test ./internal/dbaccess/ -run TestChaos_Manager_EvictionUseAfterClose -v
func TestChaos_Manager_EvictionUseAfterClose(t *testing.T) {
	url := chaosPGURL()
	if url == "" {
		t.Skip("set PGVEC_URL or PG_CHAOS_URL")
	}
	ctx := context.Background()
	mgr := NewManager(1, 30*time.Minute) // bound = 1 connection across ALL apps
	defer mgr.Shutdown()

	cfg := ConnConfig{Name: "prod", Kind: "postgres", DSN: url,
		Security: SecurityPolicy{Mode: "read_only"}}

	// App A opens and holds its connection.
	dbA, err := mgr.Named(ctx, "appA", cfg)
	if err != nil {
		t.Fatalf("appA open: %v", err)
	}
	if _, err := dbA.Query(ctx, "SELECT 1"); err != nil {
		t.Fatalf("appA baseline query: %v", err)
	}

	// App B opens its connection -> pool now has 2 > max(1) -> evictLocked closes
	// the LRU, which is appA's (A.usedAt < B.usedAt). A still holds dbA.
	if _, err := mgr.Named(ctx, "appB", cfg); err != nil {
		t.Fatalf("appB open: %v", err)
	}

	// App A, still holding its handle, issues another query. The underlying
	// *sql.DB was Closed by eviction -> "sql: database is closed".
	_, qerr := dbA.Query(ctx, "SELECT 1")
	t.Logf("appA query AFTER appB evicted it: err=%v", qerr)
	if qerr == nil {
		t.Fatalf("UNEXPECTED: appA query succeeded; eviction did not close A's conn (pool bound not hit?)")
	}
	if !strings.Contains(strings.ToLower(qerr.Error()), "closed") {
		t.Logf("NOTE: error is not the expected 'closed' string but A's conn was evicted: %v", qerr)
	}
	t.Logf("CONFIRMED CROSS-APP USE-AFTER-CLOSE: app B opening a connection closed app A's in-use connection (Manager has no refcount). In the database module the shared Manager spans ALL apps, so one app exceeding the pool bound (or a TTL expiry mid-use) breaks another app's live query: %v", qerr)
}

// TestChaos_Manager_ConcurrentNamed_DoubleOpen proves concurrent Named() for the
// SAME key opens N physical connections (no single-flight) and hands an
// already-closed handle to the losers: store() closes the N-1 that lost the
// race. A caller that received a loser handle sees "database is closed".
func TestChaos_Manager_ConcurrentNamed_DoubleOpen(t *testing.T) {
	url := chaosPGURL()
	if url == "" {
		t.Skip("set PGVEC_URL or PG_CHAOS_URL")
	}
	ctx := context.Background()
	mgr := NewManager(256, 30*time.Minute)
	defer mgr.Shutdown()
	cfg := ConnConfig{Name: "shared", Kind: "postgres", DSN: url,
		Security: SecurityPolicy{Mode: "read_only"}}

	const n = 20
	var wg sync.WaitGroup
	handles := make([]DB, n)
	openErrs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db, err := mgr.Named(ctx, "raceApp", cfg)
			handles[i], openErrs[i] = db, err
		}(i)
	}
	wg.Wait()

	// Now query through every returned handle. Losers (whose db was closed by
	// store()) will fail with "database is closed".
	var closed, ok int
	for i := 0; i < n; i++ {
		if openErrs[i] != nil || handles[i] == nil {
			continue
		}
		if _, err := handles[i].Query(ctx, "SELECT 1"); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "closed") {
				closed++
			}
		} else {
			ok++
		}
	}
	t.Logf("RESULT: %d/%d returned handles usable, %d/%d returned an ALREADY-CLOSED handle", ok, n, closed, n)
	if closed > 0 {
		t.Logf("CONFIRMED DOUBLE-OPEN: %d concurrent first-time Named() calls each opened a physical conn; store() kept one and CLOSED the rest, handing closed handles to the racing callers. No single-flight in Manager.Named (manager.go:45-61).", closed)
	} else {
		t.Logf("NOTE: no closed handle observed this run (the race did not interleave); re-run. Code path still lacks single-flight.")
	}
}
