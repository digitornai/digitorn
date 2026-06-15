package rag

import (
	"math"
	"sync"
	"time"
)

type CacheConfig struct {
	Enabled    bool    `json:"enabled"`
	Threshold  float64 `json:"threshold"`
	TTLSeconds int     `json:"ttl_seconds"`
	MaxEntries int     `json:"max_entries"`
}

type cacheEntry struct {
	kb   string
	acl  string
	topK int
	vec  []float32
	gen  uint64
	hits []SearchHit
	at   time.Time
}

type semCache struct {
	mu        sync.Mutex
	threshold float64
	ttl       time.Duration
	max       int
	gen       map[string]uint64
	entries   []*cacheEntry
}

func newSemCache(cfg CacheConfig) *semCache {
	c := &semCache{
		threshold: cfg.Threshold,
		max:       cfg.MaxEntries,
		gen:       map[string]uint64{},
	}
	if c.threshold <= 0 {
		c.threshold = 0.97
	}
	if c.max <= 0 {
		c.max = 512
	}
	if cfg.TTLSeconds > 0 {
		c.ttl = time.Duration(cfg.TTLSeconds) * time.Second
	}
	return c
}

func (c *semCache) bump(kb string) {
	c.mu.Lock()
	c.gen[kb]++
	c.mu.Unlock()
}

func (c *semCache) get(kb, acl string, topK int, vec []float32) ([]SearchHit, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.gen[kb]
	now := time.Now()
	for _, e := range c.entries {
		if e.kb != kb || e.acl != acl || e.topK != topK || e.gen != cur {
			continue
		}
		if c.ttl > 0 && now.Sub(e.at) > c.ttl {
			continue
		}
		if cosineCache(vec, e.vec) >= float32(c.threshold) {
			out := make([]SearchHit, len(e.hits))
			copy(out, e.hits)
			return out, true
		}
	}
	return nil, false
}

func (c *semCache) put(kb, acl string, topK int, vec []float32, hits []SearchHit) {
	c.mu.Lock()
	defer c.mu.Unlock()
	stored := make([]SearchHit, len(hits))
	copy(stored, hits)
	c.entries = append(c.entries, &cacheEntry{
		kb: kb, acl: acl, topK: topK, vec: vec, gen: c.gen[kb], hits: stored, at: time.Now(),
	})
	if len(c.entries) > c.max {
		c.entries = c.entries[len(c.entries)-c.max:]
	}
}

func cosineCache(a, b []float32) float32 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	var dot, na, nb float64
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}
