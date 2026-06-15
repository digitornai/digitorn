// Package cron is the cron adapter: it fires an Event on a 5-field schedule
// (minute hour day-of-month month day-of-week). The parser is self-contained
// (no external dependency) and supports *, */step, a-b ranges and a,b lists.
package cron

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mbathepaul/digitorn/internal/background/adapter"
)

// Schedule is a parsed 5-field cron expression.
type Schedule struct {
	min, hour, dom, month, dow map[int]bool
	domStar, dowStar           bool
}

type fieldDef struct {
	lo, hi int
}

var fields = []fieldDef{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}

// Parse parses "m h dom month dow". dow 0 and 7 both mean Sunday.
func Parse(expr string) (*Schedule, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf("cron: expected 5 fields, got %d in %q", len(parts), expr)
	}
	sets := make([]map[int]bool, 5)
	for i, p := range parts {
		s, err := parseField(p, fields[i].lo, fields[i].hi)
		if err != nil {
			return nil, fmt.Errorf("cron: field %d (%q): %w", i, p, err)
		}
		sets[i] = s
	}
	// dow 7 → 0 (Sunday).
	if sets[4][7] {
		sets[4][0] = true
		delete(sets[4], 7)
	}
	return &Schedule{
		min: sets[0], hour: sets[1], dom: sets[2], month: sets[3], dow: sets[4],
		domStar: parts[2] == "*", dowStar: parts[4] == "*",
	}, nil
}

func parseField(f string, lo, hi int) (map[int]bool, error) {
	out := map[int]bool{}
	for _, part := range strings.Split(f, ",") {
		step := 1
		rng := part
		if i := strings.IndexByte(part, '/'); i >= 0 {
			s, err := strconv.Atoi(part[i+1:])
			if err != nil || s < 1 {
				return nil, fmt.Errorf("bad step %q", part)
			}
			step = s
			rng = part[:i]
		}
		var start, end int
		switch {
		case rng == "*":
			start, end = lo, hi
		case strings.IndexByte(rng, '-') >= 0:
			ab := strings.SplitN(rng, "-", 2)
			a, err1 := strconv.Atoi(ab[0])
			b, err2 := strconv.Atoi(ab[1])
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("bad range %q", rng)
			}
			start, end = a, b
		default:
			v, err := strconv.Atoi(rng)
			if err != nil {
				return nil, fmt.Errorf("bad value %q", rng)
			}
			start, end = v, v
		}
		if start < lo || end > hi || start > end {
			return nil, fmt.Errorf("out of range %d-%d (allowed %d-%d)", start, end, lo, hi)
		}
		for v := start; v <= end; v += step {
			out[v] = true
		}
	}
	return out, nil
}

// Next returns the first matching minute strictly after `after`. Returns zero
// time if nothing matches within ~4 years (a malformed-but-parsed schedule).
func (s *Schedule) Next(after time.Time) time.Time {
	t := after.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(4, 0, 0)
	for ; t.Before(limit); t = t.Add(time.Minute) {
		if s.matches(t) {
			return t
		}
	}
	return time.Time{}
}

func (s *Schedule) matches(t time.Time) bool {
	if !s.min[t.Minute()] || !s.hour[t.Hour()] || !s.month[int(t.Month())] {
		return false
	}
	domOK := s.dom[t.Day()]
	dowOK := s.dow[int(t.Weekday())]
	// Vixie semantics: if BOTH day fields are restricted, a day matches when
	// EITHER matches; otherwise both must match (the non-restricted one is *).
	if !s.domStar && !s.dowStar {
		return domOK || dowOK
	}
	return domOK && dowOK
}

// Provider is one armed cron schedule.
type Provider struct {
	Name     string
	Schedule *Schedule
}

// Adapter fires Events on schedules. It is inbound-only (Send is a no-op). It
// supports RUNTIME arming (Arm): the ops API can add a new schedule to the live
// adapter without a restart. State is mutex-guarded since Arm races Start.
type Adapter struct {
	mu        sync.Mutex
	providers []Provider
	armed     map[string]bool // provider names with a running goroutine
	now       func() time.Time
	ctx       context.Context // non-nil once Start is running
	sink      adapter.Sink
}

// New builds a cron adapter over the given providers (may be empty — schedules
// can be added later via Arm).
func New(providers []Provider) *Adapter {
	return &Adapter{providers: providers, armed: map[string]bool{}, now: time.Now}
}

func (a *Adapter) Name() string { return "cron" }

// Send is a no-op: cron is inbound-only.
func (a *Adapter) Send(context.Context, map[string]any, string) error { return nil }

// Start runs one timer goroutine per provider until ctx is cancelled. Each fire
// is an Event whose DedupKey is the fire minute, so a duplicate wake at the same
// minute is de-duplicated by the durable intake. After Start, Arm can add more.
func (a *Adapter) Start(ctx context.Context, sink adapter.Sink) error {
	a.mu.Lock()
	a.ctx, a.sink = ctx, sink
	for _, p := range a.providers {
		if !a.armed[p.Name] {
			a.armed[p.Name] = true
			go a.run(ctx, p, sink)
		}
	}
	a.mu.Unlock()
	<-ctx.Done()
	return nil
}

// Arm adds (or, before Start, queues) a schedule on the live adapter — the hot
// "program a cron at runtime" path. Idempotent by provider name: an already-armed
// name is left running unchanged (re-scheduling needs a restart). Returns true
// when a new firing goroutine was launched.
func (a *Adapter) Arm(p Provider) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	replaced := false
	for i := range a.providers {
		if a.providers[i].Name == p.Name {
			a.providers[i] = p
			replaced = true
			break
		}
	}
	if !replaced {
		a.providers = append(a.providers, p)
	}
	if a.ctx == nil || a.armed[p.Name] {
		return false // not started yet (will arm on Start), or already firing
	}
	a.armed[p.Name] = true
	go a.run(a.ctx, p, a.sink)
	return true
}

func (a *Adapter) run(ctx context.Context, p Provider, sink adapter.Sink) {
	for {
		next := p.Schedule.Next(a.now())
		if next.IsZero() {
			return // unsatisfiable schedule
		}
		wait := time.Until(next)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		fire := next.UTC().Format("2006-01-02T15:04")
		_ = sink(ctx, adapter.Event{
			Provider: p.Name,
			Adapter:  "cron",
			DedupKey: p.Name + ":" + fire,
			Source:   "cron",
			Payload:  map[string]any{"scheduled_for": fire},
		})
	}
}
