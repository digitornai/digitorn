// Package policy implements the seven security gates of the Digitorn
// runtime as documented in the language reference (docs-site/docs/
// language/11-security.md) and tutorials (docs-site/docs/tutorial/
// security-*.md).
//
// The doc is the source of truth. Each gate function below maps 1:1
// to a documented gate (0, 1a, 1b, 2, 3, 4, 5, 6).
//
// Gates 5 (data classification) and 6 (per-action rate_limit) exist as
// dead code in the Python reference (their trigger params are never
// populated by any production path). digitorn implements them FOR REAL
// with a proper YAML surface (capabilities.max_data_classification and
// capabilities.rate_limits) — a clean-slate fix, not a 1:1 port :
//   - gate 5 is pure and runs in the shared gateChain (so it filters
//     over-classified tools at schema-build AND blocks at runtime).
//   - gate 6 is STATEFUL (a sliding-window limiter) and RUNTIME-ONLY :
//     it lives in RunGates behind a non-nil PolicyContext.RateLimiter,
//     so it never consumes budget while merely building the tool list.
//
// Two execution modes use the same gate functions :
//
//   - Schema-build time : BuildAgentToolset (SG-3) walks every known
//     action and runs the gates with Caller=LLM. Anything that returns
//     Deny is filtered out of the tool list the LLM sees. This is the
//     documented primary defence ("a schema filter denies the model
//     the choice in the first place" - security-02-gates.md).
//   - Runtime : RunGates (SG-4) runs the same functions on every
//     ToolInvocation just before dispatch. Hooks, setup pipelines and
//     channel callers bypass the LLM-specific gates (1a, 1b, 2, 3,
//     approve) ; deny always applies ; gate 0 always applies.
//
// All gate functions are pure : no I/O, no global state, no logging.
// Logging and the audit event (EventSecurityDecision, SG-6) are
// emitted by the caller based on the returned Decision.
package policy

import (
	"strings"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
)

// CallerKind identifies WHO is invoking a tool. The gates bypass
// rules differ per caller — only the LLM goes through every gate ;
// hooks/setup/channels skip the LLM-specific filters.
//
// Mapping to the doc Python (security-04-hidden-vs-deny.md "Callable
// from setup pipelines, hooks, channels" column) :
//
//   - CallerLLM     → every gate applies
//   - CallerHook    → gate 0 + gate 4(deny) only
//   - CallerSetup   → gate 0 + gate 4(deny) only
//   - CallerChannel → gate 0 + gate 4(deny) only
//   - CallerInternal → bypass everything (system modules, meta-tools)
type CallerKind int

const (
	CallerUnknown CallerKind = iota
	CallerLLM
	CallerHook
	CallerSetup
	CallerChannel
	CallerInternal
)

// IsLLM is the most-used predicate in gate code : "should this gate
// actually run, or should it bypass because the caller isn't the LLM ?"
// Defined as a method so a future addition (e.g. a CallerSubAgent that
// behaves like LLM) only changes one place.
func (c CallerKind) IsLLM() bool { return c == CallerLLM }

// String renders the caller for audit rows.
func (c CallerKind) String() string {
	switch c {
	case CallerLLM:
		return "llm"
	case CallerHook:
		return "hook"
	case CallerSetup:
		return "setup"
	case CallerChannel:
		return "channel"
	case CallerInternal:
		return "internal"
	default:
		return "unknown"
	}
}

// DecisionKind is the result class of a gate evaluation. The first
// gate that returns Deny or NeedsApproval stops the sequence. Allow
// means "this gate passed, run the next one".
type DecisionKind int

const (
	DecisionAllow DecisionKind = iota
	DecisionDeny
	DecisionNeedsApproval
)

func (d DecisionKind) String() string {
	switch d {
	case DecisionAllow:
		return "allow"
	case DecisionDeny:
		return "deny"
	case DecisionNeedsApproval:
		return "needs_approval"
	default:
		return "unknown"
	}
}

// GateCode is the forensic identifier of the gate that produced a
// decision. Matches the Python audit codes exactly (security-02-gates.md
// gate node labels) so cross-stack log analysis works.
type GateCode string

const (
	GateInactive       GateCode = "gate0_inactive"
	GateModule         GateCode = "gate1a_module"
	GateMCPServer      GateCode = "gate1c_mcp_server"
	GateHidden         GateCode = "gate1b_hidden"
	GateRisk           GateCode = "gate2_risk"
	GatePermissions    GateCode = "gate3_permissions"
	GatePolicy         GateCode = "gate4_policy"
	GateClassification GateCode = "gate5_classification"
	GateRateLimit      GateCode = "gate6_rate_limit"
)

// Decision is what a gate returns. Pure value type — no I/O happens
// when one is built. The Reason is a short human-readable string the
// audit log and the LLM-facing error use. GateCode lets the caller
// emit the right forensic event without a string-match.
//
// For DecisionNeedsApproval, Reason is the "reason" string from the
// matching CapabilityGrant.Reason (security-01-approval.md) which is
// surfaced to the human approver.
type Decision struct {
	Kind   DecisionKind
	Gate   GateCode
	Reason string
}

// IsDeny is shorthand used by the schema-build filter loop.
func (d Decision) IsDeny() bool { return d.Kind == DecisionDeny }

// IsBlocking is true when the gate stops the sequence (Deny or
// NeedsApproval). Allow does not stop the sequence.
func (d Decision) IsBlocking() bool {
	return d.Kind == DecisionDeny || d.Kind == DecisionNeedsApproval
}

// allow returns the canonical Allow decision for a gate. Defined as a
// helper so every gate produces identically-shaped Allow values.
func allow(gate GateCode) Decision {
	return Decision{Kind: DecisionAllow, Gate: gate}
}

// deny builds a Deny decision with the given reason. Reason must be
// non-empty — the audit row and the synthetic tool_result both
// surface it to the agent.
func deny(gate GateCode, reason string) Decision {
	return Decision{Kind: DecisionDeny, Gate: gate, Reason: reason}
}

// needsApproval builds a NeedsApproval decision. Only gate 4
// produces this kind.
func needsApproval(gate GateCode, reason string) Decision {
	return Decision{Kind: DecisionNeedsApproval, Gate: gate, Reason: reason}
}

// Invocation is the input every gate gets. It carries the routing
// information (caller, app, agent), the action being requested
// (module + action), and the resolved manifest (for risk_level and
// required_permissions). The PolicyContext (separate type) carries
// the static capability/manifest data so gates stay pure.
type Invocation struct {
	// Routing
	Caller    CallerKind
	AppID     string
	AgentID   string
	SessionID string
	UserID    string

	// Action being requested. Both forms are required because some
	// callers know the FQN string "module.action" while others have
	// them already split.
	Module string
	Action string
}

// FQN returns "module.action" — the canonical name used everywhere
// in the audit log, the tool index and the LLM-visible tool list.
func (i Invocation) FQN() string {
	if i.Module == "" {
		return i.Action
	}
	if i.Action == "" {
		return i.Module
	}
	return i.Module + "." + i.Action
}

// PolicyContext is the static configuration a gate needs to make a
// decision. Built once per app version and shared by all sessions.
// The fields are pointers so gates can spot a nil quickly without
// repeated map lookups.
//
// AgentModulesAllowed is the resolved set of (module → actions allowed)
// for THIS agent, taking into account both app-level Capabilities and
// the per-agent modules subset. Resolution is done outside the gates
// (in SG-3) so the gates stay O(1) lookups.
type PolicyContext struct {
	AppActive    bool
	Capabilities *schema.CapabilitiesConfig

	// AgentModules : nil = agent has no module restriction (inherits
	// app capabilities) ; non-nil = strict subset of app modules.
	// Resolution into a lookup map happens in SG-3.
	AgentModules map[string]AgentModuleAccess

	// ToolSpec is the per-action spec resolved from the module's
	// manifest. Carries RiskLevel and Permissions. nil = action not
	// found in any module → gate1a fails with "unknown action".
	ToolSpec *tool.Spec

	// RateLimiter is the per-app sliding-window limiter for gate 6. It is
	// set ONLY on the runtime path (DefaultPolicyEvaluator) — nil at
	// schema-build, so building the tool list never consumes rate budget.
	// Stateful: RunGates calls it (the only gate that mutates state).
	RateLimiter *RateLimiter

	// MCPAllowedServers enforces tools.modules.mcp.constraints.allowed_servers
	// at runtime (gate 1c). nil = no restriction (every connected MCP server is
	// callable). Non-nil = ONLY the listed servers' virtual tools may run; a
	// tool of an unlisted mcp_<server> is denied — exactly as gate 1a restricts
	// modules. An empty (non-nil) set therefore denies every MCP server. Only
	// affects mcp_<server> modules; native modules ignore it.
	MCPAllowedServers map[string]struct{}
}

// AgentModuleAccess captures one entry in the agent's resolved
// module list. AllActions=true means the agent can call every action
// of this module (the YAML form `modules: [shell]`). AllActions=false
// means only Actions are allowed (`modules: [{filesystem: [read]}]`).
//
// Exported because SG-3 (BuildAgentToolset) resolves AgentModules
// from schema.AgentModules and returns this shape ; tests construct
// it directly to drive gate behaviour without going through SG-3.
type AgentModuleAccess struct {
	AllActions bool
	Actions    map[string]struct{}
}

// CanAgentCall returns true if the agent's resolved module list
// allows (module, action). Used by gate1a_module. nil context.AgentModules
// means "no restriction" (the agent inherits the app's full capability
// surface, modulo other gates).
func (c PolicyContext) CanAgentCall(module, action string) bool {
	if c.AgentModules == nil {
		return true
	}
	access, ok := c.AgentModules[module]
	if !ok {
		// MCP virtual modules are named mcp_<server>; the concrete servers an
		// app connects aren't known when the agent declares its modules, so
		// declaring the umbrella `mcp` module grants every mcp_<server> the app
		// materialises. Without this the per-agent toolset filter (SG-3) and
		// gate 1a drop every MCP virtual tool, and the agent never sees them.
		if strings.HasPrefix(module, "mcp_") {
			access, ok = c.AgentModules["mcp"]
		}
		// Pieces virtual modules are named ap_<piece>; same umbrella pattern:
		// declaring `pieces` grants every ap_<piece> virtual module.
		if !ok && strings.HasPrefix(module, "ap_") {
			access, ok = c.AgentModules["pieces"]
		}
		if !ok {
			return false
		}
	}
	if access.AllActions {
		return true
	}
	_, ok = access.Actions[action]
	return ok
}
