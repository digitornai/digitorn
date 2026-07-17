package turn

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

var ErrPoolFull = errors.New("turn: pool full")

type PoolConfig struct {
	GlobalCap int

	PerAppCap int

	PerUserCap int
}

type PoolStats struct {
	GlobalCap      int
	GlobalInFlight int
	AppCap         int
	AppCount       int
	UserCap        int
	UserCount      int
}

type Token struct {
	pool      *Pool
	appID     string
	userID    string
	appEntry  *semEntry
	userEntry *semEntry
	released  atomic.Bool
}

func (t *Token) Release() {
	if t == nil {
		return
	}
	if !t.released.CompareAndSwap(false, true) {
		return
	}
	t.pool.release(t)
}

type Pool struct {
	cfg PoolConfig

	globalSem chan struct{}

	mu       sync.RWMutex
	appSems  map[string]*semEntry
	userSems map[string]*semEntry
}

type semEntry struct {
	ch  chan struct{}
	ref atomic.Int64
}

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

func (p *Pool) Acquire(ctx context.Context, appID, userID string) (*Token, error) {
	userEntry := p.getUserEntry(userID)
	if userEntry != nil {
		if err := acquire(ctx, userEntry.ch); err != nil {
			p.putUserEntry(userID, userEntry)
			return nil, err
		}
	}
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

func acquire(ctx context.Context, sem chan struct{}) error {
	select {
	case sem <- struct{}{}:
		return nil
	default:
	}
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: %v", ErrPoolFull, ctx.Err())
	}
}
