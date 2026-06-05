package tokencount

import (
	"crypto/sha256"
	"sync"
)

// defaultCacheMaxEntries bounds the content-addressed cache so it can never
// grow without limit (the lesson from the toolmw cache). ~1M unique strings is
// generous : real workloads repeat system prompts / tool schemas heavily, so
// the hit rate is high long before the cap. On overflow new entries are simply
// not cached (still counted) — correctness is unaffected, only the hit rate.
const defaultCacheMaxEntries = 1 << 20

// countCache memoises token counts keyed by sha256(family || text). The value
// is a scalar count and the key is a hash, so an entry carries NO session /
// user / agent identity — sharing it across sessions cannot leak content. The
// mapping is immutable : a (family, text) pair always has the same count, so
// entries are never invalidated.
type countCache struct {
	mu  sync.RWMutex
	m   map[[32]byte]int
	max int
}

func newCountCache(max int) *countCache {
	return &countCache{m: make(map[[32]byte]int), max: max}
}

func cacheKey(family, text string) [32]byte {
	h := sha256.New()
	_, _ = h.Write([]byte(family))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(text))
	var k [32]byte
	copy(k[:], h.Sum(nil))
	return k
}

func (c *countCache) get(family, text string) (int, bool) {
	k := cacheKey(family, text)
	c.mu.RLock()
	n, ok := c.m[k]
	c.mu.RUnlock()
	return n, ok
}

func (c *countCache) put(family, text string, n int) {
	k := cacheKey(family, text)
	c.mu.Lock()
	if len(c.m) < c.max {
		c.m[k] = n
	}
	c.mu.Unlock()
}

func (c *countCache) size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.m)
}
