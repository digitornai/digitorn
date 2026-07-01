package runtime_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/hooks"
)

// =====================================================================
// UT-L4 — Hook engine under load
//
// Spawns hundreds of hooks ; fires them at high frequency ;
// verifies cooldown + max_fires hold under contention ; benches
// the priority-sort + condition-eval hot path.
// =====================================================================

type stressCounter struct{ n atomic.Int64 }

func (s *stressCounter) Info(string, ...any)  { s.n.Add(1) }
func (s *stressCounter) Warn(string, ...any)  {}
func (s *stressCounter) Error(string, ...any) {}

func TestHookStress_1000HooksConcurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("hook stress slow under -short")
	}
	const (
		nHooks = 1000
		nFires = 100
		nGoros = 64
	)

	// Build a hook set with mixed priorities. All conditions are
	// `always` so we measure pure dispatch overhead — adding a
	// tool_name match here would short-circuit on empty ToolName
	// (correct behaviour for an empty payload, but unrelated to
	// the stress we want to measure).
	hooks_ := make([]schema.Hook, 0, nHooks)
	for i := 0; i < nHooks; i++ {
		priority := 100 + (i % 100) // priorities 100-199
		hooks_ = append(hooks_, schema.Hook{
			ID:        fmt.Sprintf("h%d", i),
			On:        schema.HookEventTurnStart,
			Priority:  priority,
			Condition: schema.HookCondition{Type: "always"},
			Action: schema.HookAction{
				Type:   "log",
				Params: map[string]any{"message": "fired"},
			},
		})
	}

	logger := &stressCounter{}
	e := hooks.New(hooks_, hooks.ActionDeps{Logger: logger})
	e.Async = false // deterministic count

	var wg sync.WaitGroup
	wg.Add(nGoros)
	start := time.Now()
	for g := 0; g < nGoros; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < nFires; i++ {
				e.Fire(context.Background(), schema.HookEventTurnStart, nil, hooks.Payload{})
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	totalFires := int64(nHooks * nFires * nGoros)
	got := logger.n.Load()
	t.Logf("hook fires : expected=%d got=%d elapsed=%v throughput=%.0f fires/s",
		totalFires, got, elapsed, float64(got)/elapsed.Seconds())
	if got != totalFires {
		t.Errorf("expected %d fires, got %d (loss : %d)", totalFires, got, totalFires-got)
	}
}

func TestHookStress_CooldownUnderContention(t *testing.T) {
	const (
		nHooks    = 50
		cooldown  = 1.0 // 1 second
		burstSize = 1000
		nGoros    = 32
	)
	hk := []schema.Hook{}
	for i := 0; i < nHooks; i++ {
		hk = append(hk, schema.Hook{
			ID:       fmt.Sprintf("h%d", i),
			On:       schema.HookEventTurnStart,
			Cooldown: cooldown,
			Action:   schema.HookAction{Type: "log", Params: map[string]any{"message": "x"}},
		})
	}
	logger := &stressCounter{}
	e := hooks.New(hk, hooks.ActionDeps{Logger: logger})
	e.Async = false

	var wg sync.WaitGroup
	wg.Add(nGoros)
	for g := 0; g < nGoros; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < burstSize; i++ {
				e.Fire(context.Background(), schema.HookEventTurnStart, nil, hooks.Payload{})
			}
		}()
	}
	wg.Wait()

	// With cooldown=1s and burst <1s, each hook should have fired
	// EXACTLY once.
	got := logger.n.Load()
	if got != int64(nHooks) {
		t.Errorf("cooldown not enforced under contention : got %d fires, want %d",
			got, nHooks)
	}
}

func TestHookStress_MaxFiresAtomicUnderRace(t *testing.T) {
	const (
		maxFires = 100
		nGoros   = 64
		perGoro  = 500
	)
	hk := []schema.Hook{{
		ID: "h1", On: schema.HookEventTurnStart,
		MaxFires: maxFires,
		Action:   schema.HookAction{Type: "log", Params: map[string]any{"message": "x"}},
	}}
	logger := &stressCounter{}
	e := hooks.New(hk, hooks.ActionDeps{Logger: logger})
	e.Async = false

	var wg sync.WaitGroup
	wg.Add(nGoros)
	for g := 0; g < nGoros; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perGoro; i++ {
				e.Fire(context.Background(), schema.HookEventTurnStart, nil, hooks.Payload{})
			}
		}()
	}
	wg.Wait()

	got := logger.n.Load()
	if got != int64(maxFires) {
		t.Errorf("max_fires not atomic : got %d fires, want exactly %d (race?)",
			got, maxFires)
	}
}
