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
// from the app's rate_limits. nil when the app declares none (gate 6 no-op).
func (e *DefaultPolicyEvaluator) limiterFor(appID string, caps *schema.CapabilitiesConfig) *policy.RateLimiter {
	if caps == nil || len(caps.RateLimits) == 0 {
		return nil
	}
	if v, ok := e.limiters.Load(appID); ok {
		return v.(*policy.RateLimiter)
	}
	actual, _ := e.limiters.LoadOrStore(appID, policy.NewRateLimiter(caps.RateLimits))
	if actual == nil {
		return nil
	}
	return actual.(*policy.RateLimiter)
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
		RateLimiter: e.limiterFor(in.AppID, caps),
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
