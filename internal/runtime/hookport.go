package runtime

import (
	"context"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/runtime/hooks"
)

// HookSource resolves the per-app hook engine + per-agent hook
// slice. The runtime is app-agnostic ; the daemon wires a source
// that maps appID → *hooks.Engine (built once per app) and that
// returns the per-agent hooks declared under agents[].hooks[].
//
// Doc-conform : app-level hooks (runtime.hooks[]) live in the
// Engine ; per-agent hooks are merged at fire time so the same
// engine serves every agent of the app without rebuilding.
type HookSource interface {
	ForApp(appID string) *hooks.Engine
	ForAgent(appID, agentID string) []schema.Hook
}

// fireHook runs every matched hook for a non-veto event and RETURNS
// the combined FireResult so the caller can apply side effects :
// transform_result mutations (tool_end) and inject_message queued
// messages (any event). Safe to call when e.Hooks is nil or the
// resolved engine is nil — returns a zero FireResult.
//
// The payload's ToolResult map (when set) is mutated IN PLACE by
// transform_result, so the caller reads it back after this returns ;
// FireResult.Modified flags whether that happened.
func (e *Engine) fireHook(ctx context.Context, appID string, agent *schemaAgent, fired schema.HookEvent, payload hooks.Payload) hooks.FireResult {
	if e.Hooks == nil {
		return hooks.FireResult{}
	}
	en := e.Hooks.ForApp(appID)
	if en == nil {
		return hooks.FireResult{}
	}
	var agentHooks []schema.Hook
	if agent != nil {
		payload.AgentID = agent.ID
		agentHooks = e.Hooks.ForAgent(appID, agent.ID)
	}
	return en.Fire(ctx, fired, agentHooks, payload)
}

// fireHookGate is the veto-aware fire path used at tool_start :
// runs every matched hook including synchronous `gate` and
// `transform_params` actions, and returns the combined gate
// decision (when any hook produced one). nil = no gate decision,
// proceed normally.
func (e *Engine) fireHookGate(ctx context.Context, appID string, agent *schemaAgent, fired schema.HookEvent, payload hooks.Payload) *hooks.GateDecision {
	if e.Hooks == nil {
		return nil
	}
	en := e.Hooks.ForApp(appID)
	if en == nil {
		return nil
	}
	var agentHooks []schema.Hook
	if agent != nil {
		payload.AgentID = agent.ID
		agentHooks = e.Hooks.ForAgent(appID, agent.ID)
	}
	res := en.Fire(ctx, fired, agentHooks, payload)
	return res.Gate
}

// FireLifecycle fires a session-lifecycle hook event that occurs
// OUTSIDE the per-turn flow — session_end (on session close/abort) and
// pre_compact (right before context compaction). The daemon's HTTP
// layer calls this from the session-delete and compaction handlers.
//
// It resolves the app's first agent so per-agent hooks merge exactly as
// they do mid-turn. There is no turn to attach to, so inject_message /
// gate / transform effects are not applied (they are meaningless for a
// closing or compacting session) ; the FireResult is returned for the
// rare caller that wants to inspect it. Best-effort and nil-safe :
// a missing app or nil hook source is a clean no-op.
func (e *Engine) FireLifecycle(ctx context.Context, event schema.HookEvent, appID, sessionID, userID string) hooks.FireResult {
	if e == nil || e.Hooks == nil || appID == "" {
		return hooks.FireResult{}
	}
	var agent *schema.Agent
	if e.Apps != nil {
		if app, err := e.Apps.Get(ctx, appID); err == nil &&
			app != nil && app.Definition != nil && len(app.Definition.Agents) > 0 {
			agent = &app.Definition.Agents[0]
		}
	}
	return e.fireHook(ctx, appID, agent, event, hooks.Payload{
		AppID: appID, SessionID: sessionID, UserID: userID,
	})
}

// schemaAgent is an alias to avoid an import cycle in this file
// (which sits beside engine.go and shares its imports).
type schemaAgent = schema.Agent
