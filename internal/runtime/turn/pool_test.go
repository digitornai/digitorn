package turn_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/runtime/turn"
)

func TestPool_AcquireRelease_HappyPath(t *testing.T) {
	p := turn.NewPool(turn.PoolConfig{GlobalCap: 4, PerAppCap: 2, PerUserCap: 1})

	tok, err := p.Acquire(context.Background(), "app-1", "user-A")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if tok == nil {
		t.Fatal("token nil on success")
	}
	stats := p.Stats("app-1", "user-A")
	if stats.GlobalInFlight != 1 || stats.AppCount != 1 || stats.UserCount != 1 {
		t.Errorf("stats after acquire: %+v", stats)
	}

	tok.Release()
	stats = p.Stats("app-1", "user-A")
	if stats.GlobalInFlight != 0 || stats.AppCount != 0 || stats.UserCount != 0 {
		t.Errorf("stats after release: %+v", stats)
	}
}

func TestPool_ReleaseIsIdempotent(t *testing.T) {
	p := turn.NewPool(turn.PoolConfig{GlobalCap: 1, PerAppCap: 1, PerUserCap: 1})
	tok, _ := p.Acquire(context.Background(), "app-1", "user-A")
	tok.Release()
	tok.Release() // must not deadlock OR leak negative counts
	tok.Release()
	stats := p.Stats("app-1", "user-A")
	if stats.GlobalInFlight != 0 {
		t.Errorf("double release corrupted counts: %+v", stats)
	}
}

func TestPool_NilTokenReleaseIsSafe(t *testing.T) {
	var tok *turn.Token
	tok.Release() // must not panic
}

func TestPool_GlobalSaturationBlocks(t *testing.T) {
	p := turn.NewPool(turn.PoolConfig{GlobalCap: 2})
	t1, _ := p.Acquire(context.Background(), "app-1", "user-A")
	t2, _ := p.Acquire(context.Background(), "app-2", "user-B")
	defer t1.Release()
	defer t2.Release()

	// Third acquire must block then time out.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	tok, err := p.Acquire(ctx, "app-3", "user-C")
	if err == nil {
		tok.Release()
		t.Fatal("expected ErrPoolFull, got success")
	}
	if !errors.Is(err, turn.ErrPoolFull) {
		t.Errorf("err = %v, want ErrPoolFull", err)
	}
}

func TestPool_PerAppSaturationBlocks(t *testing.T) {
	p := turn.NewPool(turn.PoolConfig{GlobalCap: 100, PerAppCap: 2, PerUserCap: 100})
	t1, _ := p.Acquire(context.Background(), "app-x", "user-A")
	t2, _ := p.Acquire(context.Background(), "app-x", "user-B")
	defer t1.Release()
	defer t2.Release()

	// Same app, different user — still blocked by app cap.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.Acquire(ctx, "app-x", "user-C"); !errors.Is(err, turn.ErrPoolFull) {
		t.Errorf("expected ErrPoolFull, got %v", err)
	}

	// Different app, still under global cap — should succeed.
	tok, err := p.Acquire(context.Background(), "app-y", "user-D")
	if err != nil {
		t.Fatalf("different app should succeed: %v", err)
	}
	tok.Release()
}

func TestPool_PerUserSaturationBlocks(t *testing.T) {
	p := turn.NewPool(turn.PoolConfig{GlobalCap: 100, PerAppCap: 100, PerUserCap: 2})
	t1, _ := p.Acquire(context.Background(), "app-1", "user-Z")
	t2, _ := p.Acquire(context.Background(), "app-2", "user-Z")
	defer t1.Release()
	defer t2.Release()

	// Same user, third app — blocked by user cap.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.Acquire(ctx, "app-3", "user-Z"); !errors.Is(err, turn.ErrPoolFull) {
		t.Errorf("expected ErrPoolFull, got %v", err)
	}

	// Different user — should succeed.
	tok, err := p.Acquire(context.Background(), "app-4", "user-Q")
	if err != nil {
		t.Fatalf("different user should succeed: %v", err)
	}
	tok.Release()
}

func TestPool_PartialAcquireReleasesAllTiers(t *testing.T) {
	// Setup : global has room (10), per-app has room (10), per-user
	// is saturated (1 already taken). Acquire must release global + app
	// slots before returning ErrPoolFull, otherwise leaks accumulate.
	p := turn.NewPool(turn.PoolConfig{GlobalCap: 10, PerAppCap: 10, PerUserCap: 1})
	t1, _ := p.Acquire(context.Background(), "app-1", "user-A")
	defer t1.Release()

	preStats := p.Stats("app-1", "user-A")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.Acquire(ctx, "app-1", "user-A"); !errors.Is(err, turn.ErrPoolFull) {
		t.Fatalf("expected ErrPoolFull, got %v", err)
	}

	postStats := p.Stats("app-1", "user-A")
	if postStats.GlobalInFlight != preStats.GlobalInFlight {
		t.Errorf("global leaked: pre=%d post=%d", preStats.GlobalInFlight, postStats.GlobalInFlight)
	}
	if postStats.AppCount != preStats.AppCount {
		t.Errorf("app leaked: pre=%d post=%d", preStats.AppCount, postStats.AppCount)
	}
}

func TestPool_ZeroCapMeansUnbounded(t *testing.T) {
	p := turn.NewPool(turn.PoolConfig{}) // all 0
	const N = 200
	tokens := make([]*turn.Token, N)
	for i := 0; i < N; i++ {
		tok, err := p.Acquire(context.Background(), "app-1", "user-A")
		if err != nil {
			t.Fatalf("zero cap should be unbounded, failed at i=%d : %v", i, err)
		}
		tokens[i] = tok
	}
	for _, tok := range tokens {
		tok.Release()
	}
}

func TestPool_EmptyAppIDOrUserIDSkipsTier(t *testing.T) {
	p := turn.NewPool(turn.PoolConfig{GlobalCap: 10, PerAppCap: 1, PerUserCap: 1})
	// Acquire with empty appID/userID = those tiers are skipped.
	t1, err := p.Acquire(context.Background(), "", "")
	if err != nil {
		t.Fatalf("empty ids should not block: %v", err)
	}
	defer t1.Release()
	t2, err := p.Acquire(context.Background(), "", "")
	if err != nil {
		t.Fatalf("second empty acquire blocked: %v", err)
	}
	defer t2.Release()
}

func TestPool_ConcurrentAcquireReleaseStable(t *testing.T) {
	p := turn.NewPool(turn.PoolConfig{GlobalCap: 50, PerAppCap: 10, PerUserCap: 5})
	const goroutines = 100
	const iterPer = 50
	var wg sync.WaitGroup
	var failures atomic.Int64
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			appID := "app-" + string(rune('A'+(id%4)))
			userID := "user-" + string(rune('A'+(id%8)))
			for i := 0; i < iterPer; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				tok, err := p.Acquire(ctx, appID, userID)
				cancel()
				if err != nil {
					failures.Add(1)
					continue
				}
				tok.Release()
			}
		}(g)
	}
	wg.Wait()

	// After all releases, everything must be back to zero.
	stats := p.Stats("app-A", "user-A")
	if stats.GlobalInFlight != 0 {
		t.Errorf("global leak after concurrent run: %d", stats.GlobalInFlight)
	}
}

func TestPool_BlocksThenSucceeds_WhenSlotFrees(t *testing.T) {
	p := turn.NewPool(turn.PoolConfig{GlobalCap: 1})
	t1, _ := p.Acquire(context.Background(), "app-1", "user-A")

	// Release after 50ms in a goroutine ; ensure waiter wakes up.
	go func() {
		time.Sleep(50 * time.Millisecond)
		t1.Release()
	}()

	start := time.Now()
	tok, err := p.Acquire(context.Background(), "app-2", "user-B")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("expected success after slot frees: %v", err)
	}
	defer tok.Release()
	if elapsed < 40*time.Millisecond {
		t.Errorf("Acquire returned too fast (%v) — did it actually block?", elapsed)
	}
}
