package policy

import (
	"fmt"
	"sync"
	"time"
)

// rateWindow is the sliding window gate 6 counts calls over.
const rateWindow = 60 * time.Second

// RateLimiter is the per-app sliding-window limiter behind gate 6. It tracks
// call timestamps per "module.action" key over a 60-second window and denies
// once a key's configured limit is reached. Concurrency-safe (the runtime
// dispatches tool calls in parallel).
//
// Limit keys: an exact "module.action" FQN, or "*" for a default applied to
// every key without an explicit entry. A limit <= 0 means unlimited.
//
// Stateful by design — this is why gate 6 lives in RunGates (runtime-only)
// and not in the pure gateChain : it must NOT run at schema-build time, where
// it would consume budget while merely listing tools.
type RateLimiter struct {
	mu      sync.Mutex
	limits  map[string]int
	def     int
	windows map[string][]time.Time
	now     func() time.Time // injectable clock (tests); defaults to time.Now
}

// NewRateLimiter builds a limiter from the YAML rate_limits map. Returns nil
// when the map is empty, so callers can cheaply skip gate 6 entirely.
func NewRateLimiter(limits map[string]int) *RateLimiter {
	if len(limits) == 0 {
		return nil
	}
	cp := make(map[string]int, len(limits))
	for k, v := range limits {
		cp[k] = v
	}
	return &RateLimiter{
		limits:  cp,
		def:     cp["*"],
		windows: make(map[string][]time.Time),
		now:     time.Now,
	}
}

// Check records an allowed call and returns "" when the action is within its
// limit. When the action is over its limit it returns a non-empty reason and
// does NOT record the call (so a denied call doesn't consume future budget).
func (r *RateLimiter) Check(module, action string) string {
	if r == nil {
		return ""
	}
	key := module + "." + action
	limit, ok := r.limits[key]
	if !ok {
		limit = r.def
	}
	if limit <= 0 {
		return ""
	}

	clock := r.now
	if clock == nil {
		clock = time.Now
	}
	now := clock()
	cutoff := now.Add(-rateWindow)

	r.mu.Lock()
	defer r.mu.Unlock()

	w := r.windows[key]
	drop := 0
	for drop < len(w) && w[drop].Before(cutoff) {
		drop++
	}
	if drop > 0 {
		w = append(w[:0], w[drop:]...)
	}

	if len(w) >= limit {
		r.windows[key] = w
		wait := int(rateWindow.Seconds() - now.Sub(w[0]).Seconds())
		if wait < 0 {
			wait = 0
		}
		return fmt.Sprintf("rate limit exceeded for %q: %d calls/minute (wait ~%ds before retrying)", key, limit, wait)
	}
	r.windows[key] = append(w, now)
	return ""
}
