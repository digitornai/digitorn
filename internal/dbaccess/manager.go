package dbaccess

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type Manager struct {
	mu    sync.Mutex
	conns map[string]*mentry
	max   int
	ttl   time.Duration
}

type mentry struct {
	db        DB
	usedAt    time.Time
	ephemeral bool
}

func NewManager(maxConns int, ttl time.Duration) *Manager {
	if maxConns <= 0 {
		maxConns = 256
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &Manager{conns: map[string]*mentry{}, max: maxConns, ttl: ttl}
}

func key(app, id string) string { return app + "\x00" + id }

func connKey(app string, cfg ConnConfig) string {
	return key(app, cfg.Name) + "\x00" + connFingerprint(cfg)
}

func connFingerprint(cfg ConnConfig) string {
	ident := struct {
		Kind     string         `json:"k"`
		DSN      string         `json:"d"`
		Security SecurityPolicy `json:"s"`
	}{cfg.Kind, cfg.DSN, cfg.Security}
	b, err := json.Marshal(ident)
	if err != nil {
		b = []byte(cfg.Kind + "\x00" + cfg.DSN)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}

func (m *Manager) Named(ctx context.Context, app string, cfg ConnConfig) (DB, error) {
	k := connKey(app, cfg)
	m.mu.Lock()
	if e, ok := m.conns[k]; ok {
		e.usedAt = time.Now()
		m.mu.Unlock()
		return e.db, nil
	}
	m.mu.Unlock()

	db, err := Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	m.store(k, &mentry{db: db, usedAt: time.Now()})
	return db, nil
}

func (m *Manager) Open(ctx context.Context, app string, cfg ConnConfig) (string, DB, error) {
	db, err := Open(ctx, cfg)
	if err != nil {
		return "", nil, err
	}
	id := "conn_" + randID()
	m.store(key(app, id), &mentry{db: db, usedAt: time.Now(), ephemeral: true})
	return id, db, nil
}

func (m *Manager) Get(app, id string) (DB, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.conns[key(app, id)]; ok {
		e.usedAt = time.Now()
		return e.db, true
	}
	return nil, false
}

func (m *Manager) Close(app, id string) error {
	m.mu.Lock()
	k := key(app, id)
	e := m.conns[k]
	if e != nil {
		delete(m.conns, k)
	} else {
		prefix := k + "\x00"
		for ck, ce := range m.conns {
			if len(ck) > len(prefix) && ck[:len(prefix)] == prefix {
				k, e = ck, ce
				delete(m.conns, ck)
				break
			}
		}
	}
	m.mu.Unlock()
	if e == nil {
		return fmt.Errorf("no such connection %q", id)
	}
	return e.db.Close()
}

func (m *Manager) store(k string, e *mentry) {
	m.mu.Lock()
	if old := m.conns[k]; old != nil {
		_ = old.db.Close()
	}
	m.conns[k] = e
	m.evictLocked(time.Now())
	m.mu.Unlock()
}

func (m *Manager) evictLocked(now time.Time) {
	for k, e := range m.conns {
		if now.Sub(e.usedAt) > m.ttl {
			_ = e.db.Close()
			delete(m.conns, k)
		}
	}
	for len(m.conns) > m.max {
		var oldestK string
		var oldest time.Time
		for k, e := range m.conns {
			if oldestK == "" || e.usedAt.Before(oldest) {
				oldestK, oldest = k, e.usedAt
			}
		}
		if oldestK == "" {
			break
		}
		_ = m.conns[oldestK].db.Close()
		delete(m.conns, oldestK)
	}
}

func (m *Manager) Shutdown() {
	m.mu.Lock()
	for k, e := range m.conns {
		_ = e.db.Close()
		delete(m.conns, k)
	}
	m.mu.Unlock()
}

func randID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
