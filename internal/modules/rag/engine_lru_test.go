package rag

import (
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/indexer"
)

type closeBackend struct {
	*fakeBackend
	closes int
}

func (c *closeBackend) Close() error { c.closes++; return nil }

func TestModule_EngineLRU_EvictsClosesDeregisters(t *testing.T) {
	m := New()
	m.maxEngines = 2
	cfg, _ := ParseConfig(nil)

	add := func(key string, usedAt time.Time) *closeBackend {
		cb := &closeBackend{fakeBackend: newFakeBackend()}
		eng := NewEngine(cfg, cb, fakeEmbedder{dim: 64}, nil)
		spec := indexer.SourceSpec{Name: key, Type: "lru-test", KB: "kb"}
		m.idx.Register(spec, ragSink{eng: eng})
		m.mu.Lock()
		m.engines[key] = &engEntry{eng: eng, specs: []indexer.SourceSpec{spec}, usedAt: usedAt}
		m.mu.Unlock()
		return cb
	}

	base := time.Now()
	cbA := add("a", base.Add(-3*time.Minute)) // oldest
	cbB := add("b", base.Add(-2*time.Minute))
	cbC := add("c", base.Add(-1*time.Minute))
	if m.idx.JobCount() != 3 {
		t.Fatalf("jobs registered = %d, want 3", m.idx.JobCount())
	}

	m.mu.Lock()
	m.evictLocked(base)
	size := len(m.engines)
	m.mu.Unlock()

	if size != 2 {
		t.Fatalf("engines after evict = %d, want 2 (maxEngines)", size)
	}
	if cbA.closes != 1 {
		t.Errorf("oldest engine 'a' backend not closed on eviction (closes=%d)", cbA.closes)
	}
	if cbB.closes != 0 || cbC.closes != 0 {
		t.Errorf("non-evicted backends closed: b=%d c=%d", cbB.closes, cbC.closes)
	}
	if m.idx.JobCount() != 2 {
		t.Errorf("jobs after evict = %d, want 2 ('a' deregistered)", m.idx.JobCount())
	}
}

func TestModule_EngineLRU_TTLEvictsIdle(t *testing.T) {
	m := New()
	m.maxEngines = 100 // not the size bound — test TTL
	cfg, _ := ParseConfig(nil)
	cb := &closeBackend{fakeBackend: newFakeBackend()}
	eng := NewEngine(cfg, cb, fakeEmbedder{dim: 64}, nil)
	m.mu.Lock()
	m.engines["idle"] = &engEntry{eng: eng, usedAt: time.Now().Add(-engineTTL - time.Minute)}
	m.evictLocked(time.Now())
	size := len(m.engines)
	m.mu.Unlock()
	if size != 0 || cb.closes != 1 {
		t.Errorf("idle engine not TTL-evicted: size=%d closes=%d", size, cb.closes)
	}
}
