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

// Manager pools open connections for ALL apps, bounded by a max count + idle
// TTL, so 10k apps cost only the hot working set — never 10k live sockets, and
// never the daemon (it lives in the worker). Two kinds of entry : config-named
// connections (opened on demand, reusable by `query` without `connect`) and
// ephemeral agent connections (from `connect`, closed by `disconnect`).
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

// connKey is the pool key for a config-declared (Named) connection. It binds
// the logical name to the connection's IDENTITY — the data source (kind, DSN)
// plus its security policy — so two callers using the SAME name but DIFFERENT
// connections (the per-user BYOK case: each user overrides the DSN) never share
// a pooled socket. Identical configs still collapse to one entry (efficient).
func connKey(app string, cfg ConnConfig) string {
	return key(app, cfg.Name) + "\x00" + connFingerprint(cfg)
}

// connFingerprint hashes the fields that determine what a connection talks to
// and what it may do. Same source + same policy → same fingerprint → shared
// pool entry; any difference → a distinct entry.
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

// Named returns the app's config-declared connection by name, opening it on
// first use and pooling it thereafter. This is what powers the "DB already
// connected at startup, agent only needs query" case.
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

// Open creates an ephemeral connection (agent `connect`) and returns its id.
func (m *Manager) Open(ctx context.Context, app string, cfg ConnConfig) (string, DB, error) {
	db, err := Open(ctx, cfg)
	if err != nil {
		return "", nil, err
	}
	id := "conn_" + randID()
	m.store(key(app, id), &mentry{db: db, usedAt: time.Now(), ephemeral: true})
	return id, db, nil
}

// Get resolves an app's connection by name OR ephemeral id.
func (m *Manager) Get(app, id string) (DB, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.conns[key(app, id)]; ok {
		e.usedAt = time.Now()
		return e.db, true
	}
	return nil, false
}

// Close closes and removes one connection (agent `disconnect`). `id` is an
// ephemeral connection id (exact key) or a config-declared connection name —
// the latter is stored under a fingerprinted key, so we fall back to matching
// any entry for that (app, name). disconnect is agent-initiated and rare, so
// the scan is not on any hot path.
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

// evictLocked closes idle (TTL) connections, then the least-recently-used over
// the size bound. Ephemeral connections are subject to TTL too (a leaked
// agent connection cannot live forever).
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

// Shutdown closes every pooled connection.
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
