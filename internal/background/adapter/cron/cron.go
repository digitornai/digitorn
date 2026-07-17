package cron

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitornai/digitorn/internal/background/adapter"
)

type Schedule struct {
	min, hour, dom, month, dow map[int]bool
	domStar, dowStar           bool
}

type fieldDef struct {
	lo, hi int
}

var fields = []fieldDef{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}

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
	// CatchUpFrom, when set, makes the provider fire once for the most recent
	// scheduled slot missed while the service was down (in (CatchUpFrom, now]).
	// The per-minute DedupKey keeps it idempotent; only the single latest missed
	// slot is replayed, so a long outage never causes a fire storm.
	CatchUpFrom time.Time
}

// Adapter fires Events on schedules. It is inbound-only (Send is a no-op). It
// supports RUNTIME arming (Arm): the ops API can add a new schedule to the live
// adapter without a restart. State is mutex-guarded since Arm races Start.
type Adapter struct {
	mu        sync.Mutex
	providers []Provider
	armed     map[string]context.CancelFunc // provider name → stop its firing goroutine
	now       func() time.Time
	ctx       context.Context // non-nil once Start is running
	sink      adapter.Sink
}

// maxCatchUpScan bounds the backward scan for a missed slot so a very stale
// CatchUpFrom (a long outage) can't spin — beyond it, catch-up is skipped.
const maxCatchUpScan = 2000

// New builds a cron adapter over the given providers (may be empty — schedules
// can be added later via Arm).
func New(providers []Provider) *Adapter {
	return &Adapter{providers: providers, armed: map[string]context.CancelFunc{}, now: time.Now}
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
		if _, ok := a.armed[p.Name]; !ok {
			pctx, cancel := context.WithCancel(ctx)
			a.armed[p.Name] = cancel
			go a.run(pctx, p, sink)
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
	if a.ctx == nil {
		return false // not started yet (will arm on Start)
	}
	if _, ok := a.armed[p.Name]; ok {
		return false // already firing
	}
	pctx, cancel := context.WithCancel(a.ctx)
	a.armed[p.Name] = cancel
	go a.run(pctx, p, a.sink)
	return true
}

// Disarm stops a provider's firing goroutine and forgets it, so a runtime
// disable (or an app being disabled) actually stops the schedule instead of
// leaving it firing until the next restart. Returns true if it was armed.
func (a *Adapter) Disarm(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cancel, ok := a.armed[name]
	if !ok {
		return false
	}
	cancel()
	delete(a.armed, name)
	for i := range a.providers {
		if a.providers[i].Name == name {
			a.providers = append(a.providers[:i], a.providers[i+1:]...)
			break
		}
	}
	return true
}

// missedSlot returns the formatted minute of the most recent scheduled fire in
// (CatchUpFrom, now], or "" when catch-up is off / nothing was missed / the gap
// is too large to scan. Bounded by maxCatchUpScan so a long outage is skipped.
func (a *Adapter) missedSlot(p Provider) string {
	if p.CatchUpFrom.IsZero() {
		return ""
	}
	now := a.now().UTC()
	last := time.Time{}
	t := p.CatchUpFrom.UTC()
	for i := 0; i < maxCatchUpScan; i++ {
		next := p.Schedule.Next(t)
		if next.IsZero() || next.After(now) {
			break
		}
		last = next
		t = next
	}
	if last.IsZero() {
		return ""
	}
	return last.UTC().Format("2006-01-02T15:04")
}

func (a *Adapter) run(ctx context.Context, p Provider, sink adapter.Sink) {
	if slot := a.missedSlot(p); slot != "" {
		_ = sink(ctx, adapter.Event{
			Provider: p.Name, Adapter: "cron", DedupKey: p.Name + ":" + slot,
			Source: "cron", Payload: map[string]any{"scheduled_for": slot, "catch_up": true},
		})
	}
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
