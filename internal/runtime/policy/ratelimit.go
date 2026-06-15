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
// Limit keys: an exact "module.action" FQN, a bare "module" (a MODULE-LEVEL
// aggregate over every action of that module — how an MCP server's
// rate_limit_rpm caps total calls to mcp_<server>), or "*" for a default
// applied to every action key without an explicit entry. A limit <= 0 means
// unlimited. A call must satisfy BOTH its module-level and its action-level
// limit; a denial by either records nothing (so a blocked call never consumes
// future budget).
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

// Check records an allowed call and returns "" when the call is within BOTH its
// module-level and action-level limit. When either is exceeded it returns a
// non-empty reason and records NOTHING (so a denied call doesn't consume future
// budget). A bare-"module" entry in limits is the aggregate cap over every
// action of that module (e.g. an MCP server's rate_limit_rpm).
func (r *RateLimiter) Check(module, action string) string {
	if r == nil {
		return ""
	}
	clock := r.now
	if clock == nil {
		clock = time.Now
	}
	now := clock()
	cutoff := now.Add(-rateWindow)

	aKey := module + "." + action
	aLimit, ok := r.limits[aKey]
	if !ok {
		aLimit = r.def
	}
	mLimit := r.limits[module] // module-level: explicit only (no "*" default)

	r.mu.Lock()
	defer r.mu.Unlock()

	// Peek both windows BEFORE recording: deny without consuming budget.
	if reason := r.peekLocked(module, mLimit, now, cutoff); reason != "" {
		return reason
	}
	if reason := r.peekLocked(aKey, aLimit, now, cutoff); reason != "" {
		return reason
	}
	// Both within limit → record both (only the ones that are actually limited).
	if mLimit > 0 {
		r.windows[module] = append(r.windows[module], now)
	}
	if aLimit > 0 {
		r.windows[aKey] = append(r.windows[aKey], now)
	}
	return ""
}

// peekLocked trims the window for key and reports a deny reason if it is at/over
// limit, WITHOUT recording the current call. limit <= 0 means unlimited (always
// ""). The caller holds r.mu.
func (r *RateLimiter) peekLocked(key string, limit int, now, cutoff time.Time) string {
	if limit <= 0 {
		return ""
	}
	w := r.windows[key]
	drop := 0
	for drop < len(w) && w[drop].Before(cutoff) {
		drop++
	}
	if drop > 0 {
		w = append(w[:0], w[drop:]...)
	}
	r.windows[key] = w
	if len(w) >= limit {
		wait := int(rateWindow.Seconds() - now.Sub(w[0]).Seconds())
		if wait < 0 {
			wait = 0
		}
		return fmt.Sprintf("rate limit exceeded for %q: %d calls/minute (wait ~%ds before retrying)", key, limit, wait)
	}
	return ""
}
