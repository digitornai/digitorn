package toolmw

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
)

// budget caps how often (and how expensively) a module may be called within a
// rolling hour. State is app-global (shared across sessions) because it guards
// a shared backend resource, not a per-user quota. A call that would breach the
// cap is rejected before it runs.
type budget struct {
	maxCallsPerHour int
	costPerCall     float64
	maxCostPerHour  float64

	mu  sync.Mutex
	log []budgetEntry // sorted by time, oldest first
}

type budgetEntry struct {
	at   time.Time
	cost float64
}

func newBudget(cfg map[string]any, deps Deps) (Middleware, error) {
	return &budget{
		maxCallsPerHour: cfgInt(cfg, "max_calls_per_hour", 0),
		costPerCall:     cfgFloat(cfg, "cost_per_call", 0),
		maxCostPerHour:  cfgFloat(cfg, "max_cost_per_hour", 0),
	}, nil
}

func (b *budget) Name() string { return "budget" }

func (b *budget) Handle(ctx context.Context, cc CallContext, next Next) (tool.Result, error) {
	now := time.Now()

	b.mu.Lock()
	b.purge(now)
	if b.maxCallsPerHour > 0 && len(b.log) >= b.maxCallsPerHour {
		b.mu.Unlock()
		return budgetReject(fmt.Sprintf("call budget exceeded for %q: %d/%d per hour",
			cc.ModuleID, len(b.log), b.maxCallsPerHour))
	}
	if b.maxCostPerHour > 0 {
		var total float64
		for _, e := range b.log {
			total += e.cost
		}
		if total+b.costPerCall > b.maxCostPerHour {
			b.mu.Unlock()
			return budgetReject(fmt.Sprintf("cost budget exceeded for %q: $%.4f/$%.4f per hour",
				cc.ModuleID, total, b.maxCostPerHour))
		}
	}
	b.mu.Unlock()

	res, err := next(ctx, cc)

	// Only successful work counts against the budget — a rejected/failed call
	// did not consume the backend resource.
	if err == nil && res.Success {
		b.mu.Lock()
		b.log = append(b.log, budgetEntry{at: now, cost: b.costPerCall})
		b.mu.Unlock()
	}
	return res, err
}

// purge drops entries older than one hour. Caller holds b.mu.
func (b *budget) purge(now time.Time) {
	cutoff := now.Add(-time.Hour)
	i := 0
	for i < len(b.log) && b.log[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		b.log = append(b.log[:0], b.log[i:]...)
	}
}

func budgetReject(msg string) (tool.Result, error) {
	return tool.Result{Success: false, Error: msg}, fmt.Errorf("toolmw: %s", msg)
}
