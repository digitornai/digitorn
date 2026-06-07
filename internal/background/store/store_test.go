package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	// File-backed SQLite with a busy timeout + WAL so the concurrent-claim test
	// exercises real write serialization instead of hitting "database is locked".
	dsn := filepath.Join(t.TempDir(), "bg.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("sql.DB: %v", err)
	}
	// SQLite is single-writer: cap the pool to 1 so concurrent writers serialize
	// at the pool instead of racing into SQLITE_BUSY. This is the production
	// setting for the local (SQLite) deployment too. (Postgres uses a real pool.)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() }) // release the file so TempDir cleanup works on Windows
	s := New(db)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func enq(t *testing.T, s *Store, dedup string) Job {
	t.Helper()
	j, created, err := s.Enqueue(context.Background(), NewJob{AppID: "app", Provider: "p", DedupKey: dedup, Payload: []byte(`{}`)})
	if err != nil {
		t.Fatalf("enqueue %s: %v", dedup, err)
	}
	if !created {
		t.Fatalf("enqueue %s: expected created=true", dedup)
	}
	return j
}

func TestEnqueue_Idempotent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	first := enq(t, s, "evt-1")

	j2, created, err := s.Enqueue(ctx, NewJob{AppID: "app", Provider: "p", DedupKey: "evt-1", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("a duplicate DedupKey must NOT create a second job")
	}
	if j2.ID != first.ID {
		t.Fatalf("duplicate returned a different job: %s vs %s", j2.ID, first.ID)
	}
	var n int64
	s.db.Model(&Job{}).Where("dedup_key = ?", "evt-1").Count(&n)
	if n != 1 {
		t.Fatalf("expected exactly 1 row for the dedup key, got %d", n)
	}
}

func TestClaim_LeasesOncePerJob(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	enq(t, s, "a")
	enq(t, s, "b")
	enq(t, s, "c")

	first, err := s.Claim(ctx, 2, time.Minute)
	if err != nil || len(first) != 2 {
		t.Fatalf("first claim = %d jobs, err=%v", len(first), err)
	}
	second, err := s.Claim(ctx, 2, time.Minute)
	if err != nil || len(second) != 1 {
		t.Fatalf("second claim must get only the remaining job, got %d", len(second))
	}
	third, err := s.Claim(ctx, 2, time.Minute)
	if err != nil || len(third) != 0 {
		t.Fatalf("nothing left to claim, got %d", len(third))
	}
}

func TestClaim_ExpiredLeaseReclaim(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	enq(t, s, "x")

	c1, _ := s.Claim(ctx, 1, 40*time.Millisecond)
	if len(c1) != 1 {
		t.Fatalf("first claim failed")
	}
	// Worker "dies" — lease lapses.
	if c2, _ := s.Claim(ctx, 1, time.Minute); len(c2) != 0 {
		t.Fatalf("must NOT reclaim before the lease expires, got %d", len(c2))
	}
	time.Sleep(80 * time.Millisecond)
	c3, _ := s.Claim(ctx, 1, time.Minute)
	if len(c3) != 1 || c3[0].ID != c1[0].ID {
		t.Fatalf("expired lease must be reclaimable (crash recovery), got %d", len(c3))
	}
	if c3[0].Attempts != 2 {
		t.Fatalf("attempts should be 2 after a reclaim, got %d", c3[0].Attempts)
	}
}

func TestComplete_NotReclaimed(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	enq(t, s, "done-me")
	c, _ := s.Claim(ctx, 1, time.Minute)
	if err := s.Complete(ctx, c[0].ID); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.Claim(ctx, 5, time.Minute); len(again) != 0 {
		t.Fatalf("a completed job must never be claimed again, got %d", len(again))
	}
	j, _ := s.Get(ctx, c[0].ID)
	if j.State != JobDone {
		t.Fatalf("state = %q, want done", j.State)
	}
}

func TestFail_TerminalVsRetry(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// terminal
	enq(t, s, "boom")
	c, _ := s.Claim(ctx, 1, time.Minute)
	if err := s.Fail(ctx, c[0].ID, "nope", 0); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.Claim(ctx, 5, time.Minute); len(again) != 0 {
		t.Fatalf("a terminally-failed job must not be reclaimed, got %d", len(again))
	}

	// retry with backoff → not claimable until run_after
	enq(t, s, "retry")
	c2, _ := s.Claim(ctx, 1, time.Minute)
	if err := s.Fail(ctx, c2[0].ID, "transient", time.Hour); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.Claim(ctx, 5, time.Minute); len(again) != 0 {
		t.Fatalf("a backed-off retry must not be claimable yet, got %d", len(again))
	}
}

// 8 goroutines hammering Claim over 200 jobs must each lease distinct jobs:
// no job claimed twice, all eventually claimed. Run with -race.
func TestClaim_ConcurrentNoDoubleLease(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const total = 200
	for i := 0; i < total; i++ {
		enq(t, s, fmt.Sprintf("j-%d", i))
	}

	var mu sync.Mutex
	seen := make(map[string]int)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				batch, err := s.Claim(ctx, 7, time.Minute)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if len(batch) == 0 {
					return
				}
				mu.Lock()
				for _, j := range batch {
					seen[j.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != total {
		t.Fatalf("claimed %d distinct jobs, want %d", len(seen), total)
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("job %s claimed %d times (double-lease!)", id, c)
		}
	}
}

func TestTriggers_UpsertListCursor(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	a := &Trigger{AppID: "A", Provider: "hook", Adapter: "webhook", Enabled: true}
	b := &Trigger{AppID: "B", Provider: "cron1", Adapter: "cron", Enabled: true}
	disabled := &Trigger{AppID: "A", Provider: "old", Adapter: "webhook", Enabled: false}
	for _, tr := range []*Trigger{a, b, disabled} {
		if err := s.UpsertTrigger(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}

	all, _ := s.ListTriggers(ctx, "")
	if len(all) != 2 {
		t.Fatalf("enabled-only list = %d, want 2", len(all))
	}
	onlyA, _ := s.ListTriggers(ctx, "A")
	if len(onlyA) != 1 || onlyA[0].ID != a.ID {
		t.Fatalf("app filter wrong: %+v", onlyA)
	}

	if err := s.SetCursor(ctx, a.ID, "cursor-42"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListTriggers(ctx, "A")
	if got[0].Cursor != "cursor-42" {
		t.Fatalf("cursor not persisted: %q", got[0].Cursor)
	}
}
