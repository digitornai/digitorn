package runtime

import (
	"context"
	"sync"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
)

// PolicyEvaluator runs the documented seven-gate security sequence
// (docs-site/docs/language/11-security.md) on every tool_call the
// LLM emits, before the engine hands the call to the Dispatcher.
//
// Opt-in : Engine.PolicyEvaluator may be nil, in which case the
// runtime skips enforcement entirely. The doc treats absence of
// capabilities as "dev/test mode (no enforcement)" so this matches
// the documented behaviour for unwired test deployments and for the
// transition period during which the daemon learns to wire the
// evaluator from app YAML.
//
// In production, the daemon wires DefaultPolicyEvaluator with :
//   - the current app's capabilities block,
//   - the agent's modules + permissions,
//   - a ToolSpecLookup that resolves the action's tool.Spec from
//     the dispatcher's catalog (so gates 2 and 3 see RiskLevel +
//     required permissions).
type PolicyEvaluator interface {
	// Evaluate computes the gate decision for one tool_call.
	// Implementations MUST be safe for concurrent calls — the
	// dispatchToolsParallel goroutines call Evaluate concurrently.
	Evaluate(ctx context.Context, in EvaluateInput) policy.Decision
}

// EvaluateInput carries everything PolicyEvaluator.Evaluate needs to
// build a policy.Invocation + policy.PolicyContext. Struct (instead
// of long argument lists) so we can extend the inputs later (e.g.
// add a CallerKind hint when hooks invoke tools in a future sprint)
// without breaking the interface.
type EvaluateInput struct {
	AppID     string
	SessionID string
	UserID    string

	Module string
	Action string

	App   *appmgr.RuntimeApp
	Agent *schema.Agent
}

// ToolSpecLookup resolves a (module, action) pair to the action's
// tool.Spec. Returns nil when the pair is unknown to the catalog.
// SG-4 uses this to provide gates 2/3 with the per-action data they
// need ; a later sprint wires a concrete lookup over the
// ModuleDispatcher.
type ToolSpecLookup interface {
	LookupToolSpec(module, action string) *tool.Spec
}

// DefaultPolicyEvaluator is the production implementation. It looks
// up the action's tool.Spec via the configured ToolSpecLookup and
// passes the assembled Invocation + PolicyContext to
// policy.RunGates. When Lookup is nil, the ToolSpec stays nil ;
// gates 2/3 then fail-closed as documented.
type DefaultPolicyEvaluator struct {
	Lookup ToolSpecLookup

	// limiters holds one stateful gate-6 rate limiter per app (keyed by
	// AppID), created lazily from the app's capabilities.rate_limits. The
	// windows persist across turns/versions so the limit is enforced over
	// the live app, not reset on every call. Concurrency-safe.
	limiters sync.Map // appID → *policy.RateLimiter
}

// limiterFor returns the per-app gate-6 rate limiter, creating it on first use
// from the app's capabilities.rate_limits MERGED with each MCP server's
// rate_limit_rpm (a module-level "mcp_<server>" cap). nil when neither source
// declares a limit (gate 6 no-op).
func (e *DefaultPolicyEvaluator) limiterFor(appID string, caps *schema.CapabilitiesConfig, app *appmgr.RuntimeApp) *policy.RateLimiter {
	if v, ok := e.limiters.Load(appID); ok {
		return v.(*policy.RateLimiter) // may be a nil *RateLimiter — Check is nil-safe
	}
	// Cache miss : build once. mcpServerRateLimits re-parses the servers config,
	// so it runs only here, not on every tool call.
	merged := map[string]int{}
	if caps != nil {
		for k, v := range caps.RateLimits {
			merged[k] = v
		}
	}
	for k, v := range mcpServerRateLimits(app) { // per-server rate_limit_rpm → module-level cap
		merged[k] = v
	}
	lim := policy.NewRateLimiter(merged) // nil *RateLimiter when merged is empty
	actual, _ := e.limiters.LoadOrStore(appID, lim)
	return actual.(*policy.RateLimiter)
}

// mcpServerRateLimits extracts each MCP server's rate_limit_rpm as a
// module-level limit keyed "mcp_<server>" (caps the TOTAL calls to that server
// across all its tools, matching the Python per-server semantics). nil when no
// MCP server declares one.
func mcpServerRateLimits(app *appmgr.RuntimeApp) map[string]int {
	mb, ok := mcpModuleBlock(app)
	if !ok || mb.Config == nil {
		return nil
	}
	servers, _ := schema.NormalizeServers(mb.Config["servers"])
	out := map[string]int{}
	for id, sc := range servers {
		if sc.RateLimitRPM > 0 {
			out["mcp_"+id] = sc.RateLimitRPM
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mcpAllowedServers resolves tools.modules.mcp.constraints.allowed_servers into
// a lookup set for gate 1c. nil = the constraint is absent (no restriction). A
// present-but-empty list yields a non-nil empty set (deny every MCP server).
func mcpAllowedServers(app *appmgr.RuntimeApp) map[string]struct{} {
	mb, ok := mcpModuleBlock(app)
	if !ok || mb.Constraints == nil {
		return nil
	}
	raw, ok := mb.Constraints["allowed_servers"]
	if !ok {
		return nil
	}
	set := map[string]struct{}{}
	for _, s := range constraintStringList(raw) {
		set[s] = struct{}{}
	}
	return set
}

// mcpModuleBlock returns the app's tools.modules.mcp block.
func mcpModuleBlock(app *appmgr.RuntimeApp) (schema.ModuleBlock, bool) {
	if app == nil || app.Definition == nil || app.Definition.Tools == nil {
		return schema.ModuleBlock{}, false
	}
	mb, ok := app.Definition.Tools.Modules["mcp"]
	return mb, ok
}

// constraintStringList coerces a YAML constraint value ([]any / []string) into
// a []string, ignoring non-string entries.
func constraintStringList(raw any) []string {
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// Evaluate runs the documented gates via policy.RunGates.
func (e *DefaultPolicyEvaluator) Evaluate(_ context.Context, in EvaluateInput) policy.Decision {
	var caps *schema.CapabilitiesConfig
	var appActive bool
	if in.App != nil {
		if in.App.Meta != nil {
			appActive = in.App.Meta.Enabled
		}
		if in.App.Definition != nil && in.App.Definition.Tools != nil && in.App.Definition.Tools.Capabilities != nil {
			caps = in.App.Definition.Tools.Capabilities
		}
	}

	var agentModules map[string]policy.AgentModuleAccess
	if in.Agent != nil {
		agentModules = policy.ResolveAgentModules(in.Agent.Modules)
	}

	var spec *tool.Spec
	if e.Lookup != nil {
		spec = e.Lookup.LookupToolSpec(in.Module, in.Action)
	}

	inv := policy.Invocation{
		Caller:    policy.CallerLLM, // SG-4 runtime path is always LLM-emitted
		AppID:     in.AppID,
		AgentID:   agentID(in.Agent),
		SessionID: in.SessionID,
		UserID:    in.UserID,
		Module:    in.Module,
		Action:    in.Action,
	}
	pc := policy.PolicyContext{
		AppActive:    appActive,
		Capabilities: caps,
		AgentModules: agentModules,
		ToolSpec:     spec,
		// Runtime-only : the per-app gate-6 limiter. Absent on the
		// schema-build path (BuildAgentToolset builds its own PolicyContext
		// with no limiter), so listing tools never consumes rate budget.
		RateLimiter: e.limiterFor(in.AppID, caps, in.App),
		// Gate 1c : per-app MCP allowed_servers allow-list. nil = no restriction.
		MCPAllowedServers: mcpAllowedServers(in.App),
	}
	return policy.RunGates(inv, pc)
}

// agentID is a nil-tolerant helper.
func agentID(a *schema.Agent) string {
	if a == nil {
		return ""
	}
	return a.ID
}
