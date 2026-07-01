package runner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/digitornai/digitorn/internal/background/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "bg.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	s := store.New(db)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func seed(t *testing.T, s *store.Store, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, _, err := s.Enqueue(context.Background(), store.NewJob{
			AppID: "app", Provider: "p", DedupKey: fmt.Sprintf("e-%d", i), Payload: []byte(`{}`),
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

type procFunc func(context.Context, store.Job) error

func (f procFunc) Process(ctx context.Context, j store.Job) error { return f(ctx, j) }

func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", msg)
}

// fast pool options for tests
func fastOpts(workers int) Options {
	return Options{Workers: workers, LeaseTTL: 5 * time.Second, PollMin: 5 * time.Millisecond, PollMax: 20 * time.Millisecond}
}

func runPool(t *testing.T, p *Pool) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("pool.Run did not return after cancel (leak)")
		}
	})
	return cancel
}

func TestPool_DrainsAll(t *testing.T) {
	s := newStore(t)
	seed(t, s, 50)
	p := New(s, procFunc(func(_ context.Context, _ store.Job) error { return nil }), fastOpts(8))
	runPool(t, p)
	// Wait on the durable outcome (Completed), not the processor's own counter,
	// which would race ahead of the store.Complete write.
	waitFor(t, 5*time.Second, func() bool { return p.Stats().Completed == 50 }, "all 50 completed")
}

func TestPool_BoundedConcurrency(t *testing.T) {
	s := newStore(t)
	seed(t, s, 40)
	const workers = 4
	var inflight, maxSeen atomic.Int64
	p := New(s, procFunc(func(_ context.Context, _ store.Job) error {
		cur := inflight.Add(1)
		for {
			m := maxSeen.Load()
			if cur <= m || maxSeen.CompareAndSwap(m, cur) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		inflight.Add(-1)
		return nil
	}), fastOpts(workers))
	runPool(t, p)
	waitFor(t, 5*time.Second, func() bool { return p.Stats().Completed == 40 }, "all processed")
	if m := maxSeen.Load(); m > workers {
		t.Fatalf("max concurrency %d exceeded the %d-worker cap", m, workers)
	}
}

func TestPool_RetryThenSucceed(t *testing.T) {
	s := newStore(t)
	seed(t, s, 1)
	p := New(s, procFunc(func(_ context.Context, j store.Job) error {
		if j.Attempts < 2 { // fails the first claim, succeeds on the retry
			return Retry(errors.New("transient"), 10*time.Millisecond)
		}
		return nil
	}), fastOpts(2))
	runPool(t, p)
	waitFor(t, 5*time.Second, func() bool { return p.Stats().Completed == 1 }, "completed after retry")
	if r := p.Stats().Retried; r < 1 {
		t.Fatalf("expected at least 1 retry, got %d", r)
	}
}

func TestPool_TerminalFail(t *testing.T) {
	s := newStore(t)
	seed(t, s, 1)
	p := New(s, procFunc(func(_ context.Context, _ store.Job) error {
		return errors.New("permanent")
	}), fastOpts(2))
	runPool(t, p)
	waitFor(t, 5*time.Second, func() bool { return p.Stats().Failed == 1 }, "terminally failed")
}

// A processor that panics must NOT take down its worker or the pool: the other
// jobs still drain.
func TestPool_PanicShielded(t *testing.T) {
	s := newStore(t)
	// one poison job + 6 good ones
	_, _, _ = s.Enqueue(context.Background(), store.NewJob{AppID: "a", Provider: "p", DedupKey: "boom", Payload: []byte(`{}`)})
	for i := 0; i < 6; i++ {
		_, _, _ = s.Enqueue(context.Background(), store.NewJob{AppID: "a", Provider: "p", DedupKey: fmt.Sprintf("ok-%d", i), Payload: []byte(`{}`)})
	}
	p := New(s, procFunc(func(_ context.Context, j store.Job) error {
		if j.DedupKey == "boom" {
			panic("kaboom")
		}
		return nil
	}), fastOpts(3))
	runPool(t, p)
	// the 6 good jobs complete despite the panicking one
	waitFor(t, 5*time.Second, func() bool { return p.Stats().Completed == 6 }, "6 good jobs drained past the panic")
	if p.Stats().Processed < 7 {
		t.Fatalf("the poison job should have been processed (and shielded), processed=%d", p.Stats().Processed)
	}
}

func TestPool_GracefulStop(t *testing.T) {
	s := newStore(t)
	seed(t, s, 5)
	p := New(s, procFunc(func(ctx context.Context, _ store.Job) error { return nil }), fastOpts(4))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return promptly after cancel")
	}
}
