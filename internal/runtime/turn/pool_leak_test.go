package turn

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func (p *Pool) keyCounts() (users, apps int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.userSems), len(p.appSems)
}

// TestPool_EntriesReclaimedAfterRelease is the regression test for the
// unbounded-map leak : the old Pool created a channel per distinct app/user id
// and never removed it, so userSems grew forever. With refcounting, once the
// last holder for a key releases (ref → 0) the entry is reclaimed.
func TestPool_EntriesReclaimedAfterRelease(t *testing.T) {
	p := NewPool(PoolConfig{GlobalCap: 1000, PerAppCap: 100, PerUserCap: 4})
	const N = 5000
	for i := 0; i < N; i++ {
		tok, err := p.Acquire(context.Background(), "app", fmt.Sprintf("user-%d", i))
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		tok.Release()
	}
	if users, apps := p.keyCounts(); users != 0 || apps != 0 {
		t.Errorf("entries leaked after %d sequential turns: users=%d apps=%d (want 0,0)", N, users, apps)
	}
}

// TestPool_EntryKeptWhileReferenced proves the refcount never deletes an entry
// that another caller still holds : with one token open on (app, userA), many
// acquire/release cycles on the SAME app must not reclaim the app entry, and it
// must vanish only after the held token is released.
func TestPool_EntryKeptWhileReferenced(t *testing.T) {
	p := NewPool(PoolConfig{GlobalCap: 100, PerAppCap: 50, PerUserCap: 50})
	held, err := p.Acquire(context.Background(), "app", "userA")
	if err != nil {
		t.Fatalf("acquire held: %v", err)
	}
	for i := 0; i < 200; i++ {
		tok, err := p.Acquire(context.Background(), "app", fmt.Sprintf("u-%d", i))
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		tok.Release()
	}
	if _, apps := p.keyCounts(); apps != 1 {
		t.Errorf("app entry must persist while a token holds it: apps=%d (want 1)", apps)
	}
	held.Release()
	if users, apps := p.keyCounts(); users != 0 || apps != 0 {
		t.Errorf("entries must vanish after the last holder releases: users=%d apps=%d", users, apps)
	}
}

// TestPool_NoLeakUnderConcurrency hammers acquire/release across many distinct
// keys in parallel, then asserts the maps fully drain — proving the
// decrement/delete path is race-free (no entry stranded, no double-delete).
func TestPool_NoLeakUnderConcurrency(t *testing.T) {
	p := NewPool(PoolConfig{GlobalCap: 256, PerAppCap: 64, PerUserCap: 8})
	const (
		workers = 64
		each    = 200
	)
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				uid := fmt.Sprintf("u-%d-%d", w, i%17) // some key reuse → ref churn
				tok, err := p.Acquire(context.Background(), fmt.Sprintf("app-%d", i%5), uid)
				if err != nil {
					t.Errorf("acquire: %v", err)
					return
				}
				tok.Release()
			}
		}(w)
	}
	wg.Wait()
	if users, apps := p.keyCounts(); users != 0 || apps != 0 {
		t.Errorf("entries leaked under concurrency: users=%d apps=%d (want 0,0)", users, apps)
	}
}
