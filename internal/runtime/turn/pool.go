package turn

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// ErrPoolFull is returned by Pool.Acquire when any of the three tiers
// (global, per-app, per-user) has no available slot AND the caller's
// context expires before one frees up. Callers MUST translate this to
// HTTP 429 (with a retry hint) — the runtime is NOT obligated to
// queue.
var ErrPoolFull = errors.New("turn: pool full")

// PoolConfig sizes the three tiers. All caps are hard ; once reached,
// Acquire blocks on ctx OR returns ErrPoolFull when ctx is already
// done. Zero on any field disables that tier (treated as unlimited).
type PoolConfig struct {
	// GlobalCap caps total concurrent turns across all apps/users on
	// this daemon. Sized for daemon RAM budget. Reasonable default :
	// 4096 (each turn ≈ 1 goroutine + transient state).
	GlobalCap int

	// PerAppCap caps concurrent turns per app_id. Prevents one buggy
	// or hot app from starving every other app. Default : 256.
	PerAppCap int

	// PerUserCap caps concurrent turns per user_id. Prevents one user
	// from starving other users of the same app. Default : 32.
	PerUserCap int
}

// PoolStats is an instantaneous view of pool occupancy. Cheap to
// compute (atomic loads only). Used by /api/daemon/stats.
type PoolStats struct {
	GlobalCap      int
	GlobalInFlight int
	AppCap         int
	AppCount       int
	UserCap        int
	UserCount      int
}

// Token is the receipt returned by Acquire. Calling Release frees ALL
// three tiers in reverse order (user → app → global). Safe to call
// Release multiple times — only the first releases.
type Token struct {
	pool      *Pool
	appID     string
	userID    string
	appEntry  *semEntry // nil when the per-app tier is disabled
	userEntry *semEntry // nil when the per-user tier is disabled
	released  atomic.Bool
}

// Release returns the slots taken from each tier. Idempotent — second
// and subsequent calls are no-ops. Always call via defer right after
// Acquire to guarantee release even on panic.
func (t *Token) Release() {
	if t == nil {
		return
	}
	if !t.released.CompareAndSwap(false, true) {
		return
	}
	t.pool.release(t)
}

// Pool grants permission to run a turn. Three semaphores in series :
// global → per-app → per-user, with reverse-order release. Acquire is
// safe under concurrent calls.
//
// Concurrency model : each tier is a buffered channel (the classic Go
// counting semaphore). Per-app and per-user maps are guarded by a
// single sync.RWMutex ; lazy entry creation on first acquire. Reads on
// the hot path are read-locked which scales linearly with cores.
//
// Why ordered acquire (global → app → user) ? Deadlock-free : every
// caller takes locks in the same order, so the standard hierarchical
// lock argument applies. Why reverse release ? Symmetric, easier to
// reason about, matches the resource pyramid.
type Pool struct {
	cfg PoolConfig

	globalSem chan struct{}

	mu       sync.RWMutex
	appSems  map[string]*semEntry
	userSems map[string]*semEntry
}

// semEntry is a per-key (app or user) counting semaphore plus a reference
// count of how many callers currently hold or wait on it. The refcount is the
// fix for the unbounded-map leak : the OLD code created a channel per distinct
// app/user id and never removed it, so userSems grew without bound over a
// daemon's lifetime (one entry per user ever seen). Now the LAST releaser
// (ref → 0, i.e. no in-flight or waiting turns for that key) removes the entry,
// so the maps track only ACTIVE keys. A later acquire for the same key creates
// a fresh entry — correct, because ref==0 guarantees the channel was empty.
type semEntry struct {
	ch  chan struct{}
	ref atomic.Int64
}

// NewPool builds a Pool from the given config. Zero caps mean
// "unbounded for that tier" (the channel is nil and acquire skips it).
func NewPool(cfg PoolConfig) *Pool {
	p := &Pool{
		cfg:      cfg,
		appSems:  make(map[string]*semEntry),
		userSems: make(map[string]*semEntry),
	}
	if cfg.GlobalCap > 0 {
		p.globalSem = make(chan struct{}, cfg.GlobalCap)
	}
	return p
}

// Acquire takes one slot from each enabled tier. Blocks until either
// (a) all three are available — returns Token, nil ; or (b) ctx
// expires — returns nil, ErrPoolFull.
//
// Tier ORDER MATTERS for fairness : we acquire user → app → global
// (most granular → least granular). A noisy user with 10× more turns
// than the cap fills the per-user queue first, NOT the global queue,
// so quiet users acquiring at the same time don't fight a backlog of
// noisy waiters at the global tier. The pre-FT bench showed that
// inverting this order (global-first) collapsed isolation : a noisy
// app drove a quiet app's p99 from 6ms → 4.5s. With user-first the
// queue forms at the leaf, not the root, and isolation holds.
//
// On partial acquire (e.g. user got, app got, global blocked), all
// already-taken slots are returned before ErrPoolFull surfaces. The
// caller never has to clean up after a failed Acquire.
func (p *Pool) Acquire(ctx context.Context, appID, userID string) (*Token, error) {
	// Tier 1 : per-user (most granular). getUserEntry bumps the entry's ref
	// (registering us as a holder/waiter) ; putUserEntry drops it.
	userEntry := p.getUserEntry(userID)
	if userEntry != nil {
		if err := acquire(ctx, userEntry.ch); err != nil {
			p.putUserEntry(userID, userEntry) // ref-- only ; no slot was taken
			return nil, err
		}
	}
	// Tier 2 : per-app.
	appEntry := p.getAppEntry(appID)
	if appEntry != nil {
		if err := acquire(ctx, appEntry.ch); err != nil {
			p.putAppEntry(appID, appEntry)
			if userEntry != nil {
				<-userEntry.ch
				p.putUserEntry(userID, userEntry)
			}
			return nil, err
		}
	}
	// Tier 3 : global (least granular ; protects daemon-wide budget).
	if p.globalSem != nil {
		if err := acquire(ctx, p.globalSem); err != nil {
			if appEntry != nil {
				<-appEntry.ch
				p.putAppEntry(appID, appEntry)
			}
			if userEntry != nil {
				<-userEntry.ch
				p.putUserEntry(userID, userEntry)
			}
			return nil, err
		}
	}
	return &Token{pool: p, appID: appID, userID: userID, appEntry: appEntry, userEntry: userEntry}, nil
}

// Stats returns instantaneous occupancy for one (app, user) pair.
// O(1) channel-length reads, no locks beyond RLock for map lookup.
// Counts the SLOTS USED, not the cap minus available.
func (p *Pool) Stats(appID, userID string) PoolStats {
	stats := PoolStats{
		GlobalCap: p.cfg.GlobalCap,
		AppCap:    p.cfg.PerAppCap,
		UserCap:   p.cfg.PerUserCap,
	}
	if p.globalSem != nil {
		stats.GlobalInFlight = len(p.globalSem)
	}
	p.mu.RLock()
	if e := p.appSems[appID]; e != nil {
		stats.AppCount = len(e.ch)
	}
	if e := p.userSems[userID]; e != nil {
		stats.UserCount = len(e.ch)
	}
	p.mu.RUnlock()
	return stats
}

// getAppEntry returns the per-app semaphore entry with its ref bumped (so it
// won't be reclaimed while we hold it), or nil when the tier is disabled. The
// ref is incremented under at least an RLock, and entries are only deleted
// under the exclusive Lock after re-checking ref==0 — so the bump can never
// race the delete. Fast path (entry exists) stays RLock + atomic add.
func (p *Pool) getAppEntry(appID string) *semEntry {
	if p.cfg.PerAppCap <= 0 || appID == "" {
		return nil
	}
	return getEntry(&p.mu, p.appSems, appID, p.cfg.PerAppCap)
}

func (p *Pool) getUserEntry(userID string) *semEntry {
	if p.cfg.PerUserCap <= 0 || userID == "" {
		return nil
	}
	return getEntry(&p.mu, p.userSems, userID, p.cfg.PerUserCap)
}

// getEntry is the shared get-or-create-with-ref-bump for both tiers.
func getEntry(mu *sync.RWMutex, m map[string]*semEntry, key string, cap int) *semEntry {
	mu.RLock()
	e := m[key]
	if e != nil {
		e.ref.Add(1)
	}
	mu.RUnlock()
	if e != nil {
		return e
	}
	mu.Lock()
	if e = m[key]; e == nil {
		e = &semEntry{ch: make(chan struct{}, cap)}
		m[key] = e
	}
	e.ref.Add(1)
	mu.Unlock()
	return e
}

func (p *Pool) putAppEntry(appID string, e *semEntry) { putEntry(&p.mu, p.appSems, appID, e) }
func (p *Pool) putUserEntry(userID string, e *semEntry) {
	putEntry(&p.mu, p.userSems, userID, e)
}

// putEntry drops one reference and, when it was the last (ref → 0), removes the
// entry from the map so it can't accumulate. The exclusive-lock re-check of
// ref==0 closes the race with a getEntry that bumped the ref back up between
// our decrement and acquiring the lock. The `cur == e` guard ensures we only
// delete the entry we actually hold (not a fresh replacement).
func putEntry(mu *sync.RWMutex, m map[string]*semEntry, key string, e *semEntry) {
	if e == nil {
		return
	}
	if e.ref.Add(-1) != 0 {
		return
	}
	mu.Lock()
	if e.ref.Load() == 0 {
		if cur := m[key]; cur == e {
			delete(m, key)
		}
	}
	mu.Unlock()
}

// release returns the slots taken from each tier and drops the per-tier refs.
// Reverse order : user → app → global (the resource pyramid, LIFO).
func (p *Pool) release(t *Token) {
	if t.userEntry != nil {
		<-t.userEntry.ch
		p.putUserEntry(t.userID, t.userEntry)
	}
	if t.appEntry != nil {
		<-t.appEntry.ch
		p.putAppEntry(t.appID, t.appEntry)
	}
	p.releaseGlobal()
}

func (p *Pool) releaseGlobal() {
	if p.globalSem != nil {
		<-p.globalSem
	}
}

// acquire is the canonical "send-or-cancel" against a buffered channel
// used as a counting semaphore. Returns ErrPoolFull when ctx is done,
// wrapped with the ctx error for debugging.
func acquire(ctx context.Context, sem chan struct{}) error {
	select {
	case sem <- struct{}{}:
		return nil
	default:
	}
	// Slow path : channel full, wait OR cancel.
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: %v", ErrPoolFull, ctx.Err())
	}
}
