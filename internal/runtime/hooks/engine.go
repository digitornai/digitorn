package hooks

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

type Sink interface {
	AppendDurable(ctx context.Context, ev sessionstore.Event) (uint64, error)
}

type ToolCaller interface {
	Call(ctx context.Context, name string, args map[string]any) (string, error)
}

type Engine struct {
	Hooks []schema.Hook

	Deps ActionDeps

	nowFn func() time.Time

	Async bool

	state sync.Map
}

type hookState struct {
	fires       atomic.Int64
	lastFiredNs atomic.Int64
}

func New(appHooks []schema.Hook, deps ActionDeps) *Engine {
	sorted := append([]schema.Hook(nil), appHooks...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return effectivePriority(sorted[i]) < effectivePriority(sorted[j])
	})
	return &Engine{
		Hooks: sorted,
		Deps:  deps,
		Async: true,
		nowFn: time.Now,
	}
}

type FireResult struct {
	Gate     *GateDecision
	Injects  []*MessageInjection
	Modified bool
}

func (e *Engine) Fire(
	ctx context.Context,
	fired schema.HookEvent,
	agentHooks []schema.Hook,
	payload Payload,
) FireResult {
	if e == nil {
		return FireResult{}
	}
	if ctx.Err() != nil {
		return FireResult{}
	}
	payload.Event = fired

	var merged []schema.Hook
	if len(agentHooks) == 0 {
		merged = e.Hooks
	} else {
		merged = mergeHookSets(e.Hooks, agentHooks)
		sort.SliceStable(merged, func(i, j int) bool {
			return effectivePriority(merged[i]) < effectivePriority(merged[j])
		})
	}
	if len(merged) == 0 {
		return FireResult{}
	}

	var (
		result FireResult
		wg     sync.WaitGroup
	)
	for _, hook := range merged {
		if !hookMatchesEvent(hook, fired) {
			continue
		}
		if !EvalCondition(hook.Condition, payload) {
			continue
		}
		if !e.allowFire(hook) {
			continue
		}
		sync := isSyncAction(hook.Action, fired)
		if sync {
			ef, err := safeRunAction(ctx, hook.Action, payload, e.Deps)
			if err != nil && e.Deps.Logger != nil {
				e.Deps.Logger.Warn("hook: action failed",
					"hook_id", hook.ID,
					"action", string(hook.Action.Type),
					"err", err.Error())
			}
			result = applyEffects(result, ef)
			if ef.Gate != nil && !ef.Gate.Allow {
				return result
			}
			continue
		}
		if e.Async {
			wg.Add(1)
			h := hook
			pl := payload.cloneForAsync()
			go func() {
				defer wg.Done()
				e.runHookAsync(ctx, h, pl)
			}()
		} else {
			e.runHookAsync(ctx, hook, payload)
		}
	}
	if !e.Async {
		wg.Wait()
	}
	return result
}

// safeRunAction runs an action and converts a panic into an error so a
// misbehaving action (or a panicking dependency : Compactor, Caller,
// Sink) can NEVER crash the turn. The async path already recovers in
// runHookAsync ; this gives the SYNCHRONOUS gate / transform_params /
// transform_result / inject_message / compact_context path the same
// hard guarantee. On panic we return zero effects + an error, so a
// crashing `gate` action fails OPEN (proceeds, logged) rather than
// wedging the turn — consistent with "a hook must never block the loop
// on its own failure". The real security gate (PolicyEvaluator) is a
// separate, earlier stage and is unaffected.
func safeRunAction(ctx context.Context, action schema.HookAction, payload Payload, deps ActionDeps) (ef ActionEffects, err error) {
	defer func() {
		if r := recover(); r != nil {
			ef = ActionEffects{}
			err = fmt.Errorf("hook action %q panicked: %v", action.Type, r)
		}
	}()
	return RunAction(ctx, action, payload, deps)
}

// runHookAsync is the goroutine body for non-blocking actions.
// Panics recovered ; errors logged ; never returned.
func (e *Engine) runHookAsync(ctx context.Context, hook schema.Hook, payload Payload) {
	defer func() {
		if r := recover(); r != nil && e.Deps.Logger != nil {
			e.Deps.Logger.Error("hook: panic",
				"hook_id", hook.ID, "panic", r)
		}
	}()
	if _, err := RunAction(ctx, hook.Action, payload, e.Deps); err != nil && e.Deps.Logger != nil {
		e.Deps.Logger.Warn("hook: async action failed",
			"hook_id", hook.ID,
			"action", string(hook.Action.Type),
			"err", err.Error())
	}
}

// allowFire enforces enabled + cooldown + max_fires, LOCK-FREE.
//
// Concurrency contract (the JVM-grade part) :
//
//   - cooldown : a single CAS on lastFiredNs claims the fire window.
//     Only the goroutine that swaps last→now proceeds ; everyone else
//     in the window is rejected. So "minimum N seconds between fires"
//     holds exactly even under massive contention.
//   - max_fires : a CAS loop on the fires counter guarantees EXACTLY
//     max_fires successful increments — never max_fires+1 — no matter
//     how many sessions race. This is the hard cap the doc promises.
//
// No mutex, no shared map lock : 10M sessions firing the same app's
// hooks contend only on the per-hook atomics, and only when that exact
// hook is hot. Different hooks never touch each other's state.
func (e *Engine) allowFire(hook schema.Hook) bool {
	if hook.Enabled != nil && !*hook.Enabled {
		return false
	}
	st := e.stateFor(hook.ID)
	now := e.now().UnixNano()

	// Cooldown gate : claim the window via CAS so concurrent fires in
	// the same window collapse to one winner.
	if hook.Cooldown > 0 {
		for {
			last := st.lastFiredNs.Load()
			if last != 0 && float64(now-last)/1e9 < hook.Cooldown {
				return false
			}
			if st.lastFiredNs.CompareAndSwap(last, now) {
				break
			}
			// Lost the race ; reload and re-check the window.
		}
	} else {
		st.lastFiredNs.Store(now)
	}

	// max_fires gate : CAS loop guarantees an exact cap.
	if hook.MaxFires > 0 {
		for {
			f := st.fires.Load()
			if f >= int64(hook.MaxFires) {
				return false
			}
			if st.fires.CompareAndSwap(f, f+1) {
				return true
			}
		}
	}
	st.fires.Add(1)
	return true
}

// stateFor returns the per-hook atomic state, creating it lock-free on
// first access.
func (e *Engine) stateFor(id string) *hookState {
	if v, ok := e.state.Load(id); ok {
		return v.(*hookState)
	}
	actual, _ := e.state.LoadOrStore(id, &hookState{})
	return actual.(*hookState)
}

// FireCount returns how many times the named hook has fired since
// Engine construction. Lock-free ; exposed for tests / observability.
func (e *Engine) FireCount(hookID string) int {
	if v, ok := e.state.Load(hookID); ok {
		return int(v.(*hookState).fires.Load())
	}
	return 0
}

// =====================================================================
// helpers
// =====================================================================

// effectivePriority returns the priority used for sort. The doc
// default is 100 ; YAML defaults the field to 0 in Go's zero value,
// so we treat 0 as "default" and use 100 instead.
func effectivePriority(h schema.Hook) int {
	if h.Priority == 0 {
		return 100
	}
	return h.Priority
}

// mergeHookSets concatenates app-level and per-agent hook slices into a
// FRESH slice. It must never return a shared backing array : the caller
// sorts the result in place, and both e.Hooks and the per-agent slice
// (returned by ForAgent from the shared AppDefinition) are read
// concurrently by other fires. Always allocating here is the price of
// correctness — and it's only paid when per-agent hooks exist (the
// no-agent common path reuses the pre-sorted e.Hooks read-only).
func mergeHookSets(app, agent []schema.Hook) []schema.Hook {
	out := make([]schema.Hook, 0, len(app)+len(agent))
	out = append(out, app...)
	out = append(out, agent...)
	return out
}

// isSyncAction reports whether an action MUST run synchronously
// for its effects to influence the engine's flow. Doc semantics :
//
//   - gate on tool_start             → may VETO the call.
//   - transform_params on tool_start → may mutate args.
//   - transform_result on tool_end   → may mutate the result.
//   - inject_message (any event)     → returns next-turn injection.
//   - compact_context (any event)    → returns compaction signal.
//   - chain (any event)              → sync IFF it WRAPS any of the
//     above ; otherwise async. A chain runs async-by-default would
//     drop the inject / gate / transform effects of its sub-actions
//     because the async path discards the ActionEffects.
//
// Everything else (log, notify, module_action, shell, pipe,
// lsp_diagnose, module_action_inject) runs async — its effects are
// observable elsewhere (log lines, session bus events, downstream
// modules) but don't feed back into the agent loop's flow.
func isSyncAction(a schema.HookAction, event schema.HookEvent) bool {
	canonical := canonicalEvent(event)
	switch a.Type {
	case "gate":
		return canonical == schema.HookEventToolStart ||
			canonical == schema.HookEventStop
	case "transform_params":
		return canonical == schema.HookEventToolStart
	case "transform_result":
		return canonical == schema.HookEventToolEnd
	case "inject_message", "compact_context", "module_action_inject":
		// module_action_inject EXISTS to inject a tool's output — its effect
		// only reaches the agent on the sync path, so it must run sync.
		return true
	case "lsp_diagnose":
		// Post-edit diagnostics must feed back into the SAME turn so the agent
		// sees its errors before continuing. Bounded by the lsp module's own
		// timeout, so it can't hang the loop.
		return canonical == schema.HookEventToolEnd
	case "chain":
		for _, sub := range chainSubActions(a.Params) {
			if isSyncAction(sub, event) {
				return true
			}
		}
	}
	return false
}

// chainSubActions decodes a chain action's `actions` list into typed
// HookActions so isSyncAction can inspect them (recursion-safe for
// nested chains). Mirrors runChain's decoding.
func chainSubActions(params map[string]any) []schema.HookAction {
	raw, ok := params["actions"].([]any)
	if !ok {
		return nil
	}
	out := make([]schema.HookAction, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		t, _ := m["type"].(string)
		p := make(map[string]any, len(m))
		for k, v := range m {
			if k != "type" {
				p[k] = v
			}
		}
		out = append(out, schema.HookAction{Type: schema.HookActionType(t), Params: p})
	}
	return out
}

// applyEffects folds the right-hand effects into the result. Injects
// accumulate so multiple inject_message hooks on the same event all
// survive ; Gate is last-wins (a veto short-circuits anyway).
func applyEffects(r FireResult, ef ActionEffects) FireResult {
	if ef.Gate != nil {
		r.Gate = ef.Gate
	}
	r.Injects = append(r.Injects, ef.Injects...)
	if ef.Modified {
		r.Modified = true
	}
	return r
}

func (e *Engine) now() time.Time {
	if e.nowFn != nil {
		return e.nowFn()
	}
	return time.Now()
}
