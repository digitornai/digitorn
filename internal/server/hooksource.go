package server

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/hooks"
)

// lifecycleFirer fires session-lifecycle hook events that occur outside
// the per-turn flow (session_end on delete, pre_compact before
// compaction). *runtime.Engine satisfies it via FireLifecycle. The
// Daemon holds it so the HTTP session handlers can fire these events
// through the SAME hook engine (and thus the same per-app cooldown /
// max_fires state) the turn loop uses.
type lifecycleFirer interface {
	FireLifecycle(ctx context.Context, event schema.HookEvent, appID, sessionID, userID string) hooks.FireResult
}

// hookSource is the production HookSource implementation that the
// runtime engine consumes. It resolves the per-app *hooks.Engine
// (built once from the app's compiled runtime.hooks[] block) and the
// per-agent hook slice (agents[i].hooks[]).
//
// Doc-conform : runtime.hooks[] is app-scoped — every session/agent
// of the same app shares the same engine instance (so the
// cooldown / max_fires state is enforced AS DOCUMENTED, ie per-app
// not per-session). Per-agent hooks are merged at fire time by the
// engine itself so the same engine serves every agent of the app
// without rebuilding.
//
// Cache discipline : a *hooks.Engine holds mutable state (fire
// counters, last-fired timestamps) that the documented cooldown /
// max_fires semantics depend on. We therefore build it once per
// appID for the lifetime of the daemon. Re-reading the app
// definition (after a hot Reload) would NOT change the engine
// because doing so silently resets the counters — a reload that
// adjusts hooks requires a daemon restart, mirroring the
// reference Python daemon's behaviour.
type hookSource struct {
	apps  appmgr.Manager
	deps  hooks.ActionDeps
	cache sync.Map // appID → *hooks.Engine
}

// newHookSource wires the production source. deps carries the
// shared ActionDeps (logger + sink + tool caller) every action
// pulls from.
func newHookSource(apps appmgr.Manager, deps hooks.ActionDeps) *hookSource {
	return &hookSource{apps: apps, deps: deps}
}

// ForApp returns the lazily-built *hooks.Engine for the given app.
// nil-safe and idempotent : repeated calls return the SAME engine
// instance so its internal state (cooldown / max_fires counters)
// persists across turns.
func (s *hookSource) ForApp(appID string) *hooks.Engine {
	if s == nil || appID == "" || s.apps == nil {
		return nil
	}
	if v, ok := s.cache.Load(appID); ok {
		return v.(*hooks.Engine)
	}
	app, err := s.apps.Get(context.Background(), appID)
	if err != nil || app == nil || app.Definition == nil {
		return nil
	}
	var rtHooks []schema.Hook
	if app.Definition.Runtime != nil {
		rtHooks = app.Definition.Runtime.Hooks
	}
	// Runtime-default hooks (e.g. the task-completion stop guard) are merged
	// ahead of the app's declared hooks so every app gets the built-in
	// guidance for free, through the same engine.
	merged := append(hooks.BuiltinHooks(), rtHooks...)
	// Auto-diagnostics: any app that grants the lsp module gets the post-edit
	// lsp_diagnose hook for free — no per-app wiring needed.
	if appGrantsLSP(app.Definition) {
		merged = append(merged, hooks.LSPDiagnoseHooks()...)
	}
	eng := hooks.New(merged, s.deps)
	actual, _ := s.cache.LoadOrStore(appID, eng)
	return actual.(*hooks.Engine)
}

// appGrantsLSP reports whether the app wires the lsp module (as a configured
// module or a capability grant) — the signal that it should receive the
// auto-diagnostics hook.
func appGrantsLSP(def *schema.AppDefinition) bool {
	if def == nil || def.Tools == nil {
		return false
	}
	if _, ok := def.Tools.Modules["lsp"]; ok {
		return true
	}
	if caps := def.Tools.Capabilities; caps != nil {
		for _, g := range caps.Grant {
			if g.Module == "lsp" {
				return true
			}
		}
	}
	return false
}

// ForAgent reads the per-agent hooks from the app's compiled
// AppDefinition. Returned slice is never mutated by callers.
func (s *hookSource) ForAgent(appID, agentID string) []schema.Hook {
	if s == nil || appID == "" || agentID == "" || s.apps == nil {
		return nil
	}
	app, err := s.apps.Get(context.Background(), appID)
	if err != nil || app == nil || app.Definition == nil {
		return nil
	}
	for i := range app.Definition.Agents {
		if app.Definition.Agents[i].ID == agentID {
			return app.Definition.Agents[i].Hooks
		}
	}
	return nil
}

// dispatchCaller adapts a runtime.ToolDispatcher to the
// hooks.ToolCaller interface. Hooks declaring the `module_action`
// action use it to route their target tool through the SAME
// dispatcher the LLM uses — same security gates, same audit row,
// same canonicalisation. Without this adapter module_action
// actions would either fail silently or bypass policy.
type dispatchCaller struct {
	d runtime.ToolDispatcher
}

// Call invokes the named tool with the given args and returns its LLM-visible
// text output (the concatenated result Parts). An "errored" outcome becomes a
// Go error so the per-hook async path can log it.
func (c dispatchCaller) Call(ctx context.Context, name string, args map[string]any) (string, error) {
	if c.d == nil {
		return "", errors.New("hooks: dispatcher not wired")
	}
	out := c.d.Dispatch(ctx, runtime.ToolInvocation{
		Name:   name,
		Args:   args,
		CallID: "hook:" + name,
	})
	if out.Status == "errored" {
		return "", errors.New(out.Error)
	}
	var sb strings.Builder
	for _, part := range out.Parts {
		sb.WriteString(part.Text)
	}
	return sb.String(), nil
}
