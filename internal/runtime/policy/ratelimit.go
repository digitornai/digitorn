package policy

import (
	"fmt"
	"sync"
	"time"
)

const rateWindow = 60 * time.Second

type RateLimiter struct {
	mu      sync.Mutex
	limits  map[string]int
	def     int
	windows map[string][]time.Time
	now     func() time.Time
}

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
	mLimit := r.limits[module]

	r.mu.Lock()
	defer r.mu.Unlock()

	if reason := r.peekLocked(module, mLimit, now, cutoff); reason != "" {
		return reason
	}
	if reason := r.peekLocked(aKey, aLimit, now, cutoff); reason != "" {
		return reason
	}
	if mLimit > 0 {
		r.windows[module] = append(r.windows[module], now)
	}
	if aLimit > 0 {
		r.windows[aKey] = append(r.windows[aKey], now)
	}
	return ""
}

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
