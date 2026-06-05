// Package injection implements the adaptive tool-injection planner
// documented in docs-site/docs/language/04-tools.md
// "Adaptive tool injection".
//
// The planner picks one of three modes per (agent, app) tuple based
// on the actual JSON size of every tool schema vs 20 % of the brain's
// context window :
//
//	direct          : full JSON schemas for every tool
//	compact_direct  : name + one-line description, schema fetched lazily
//	discovery       : only the 9 meta-tools + 1 background primitive,
//	                  domain tools reached via execute_tool
//
// The doc's reference algorithm (verbatim) :
//
//	budget         = context_window * 0.20                  # _MAX_CONTEXT_RATIO
//	tool_tokens    = sum(len(json.dumps(t)) // 4 for t in tools)
//	                 # fallback: total_tools * 200 when direct_tools is empty
//	                 # (_FALLBACK_TOKENS_PER_TOOL)
//	compact_tokens = total_tools * 30                       # name + one-liner per tool
//
//	if tool_tokens <= budget    → "direct"
//	elif compact_tokens <= budget → "compact_direct"
//	else                          → "discovery"
//
// `runtime.tool_injection` in YAML short-circuits the algorithm.
//
// CB-2 owns the planner. CB-3 wires the meta-tools to a dispatcher
// so they actually execute (search_tools / execute_tool / etc.).
package injection

import (
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/llm"
)

// Mode is one of the three documented injection modes. Mirrors
// schema.ToolInjection ; redeclared here so the planner package
// doesn't leak the compiler/schema enum into runtime call sites.
type Mode string

const (
	ModeDirect        Mode = "direct"
	ModeCompactDirect Mode = "compact_direct"
	ModeDiscovery     Mode = "discovery"
)

// AllModes is every legal injection mode. Useful for tests that need
// to iterate over the space.
var AllModes = []Mode{ModeDirect, ModeCompactDirect, ModeDiscovery}

// Decision is what the planner returns. Tools is the list to ship in
// ChatRequest.Tools ; Mode lets the caller (the system prompt
// assembler, the audit log) know which formula produced the list.
//
// ContextWindow / Budget / ToolTokens / CompactTokens are the
// intermediate values used to make the decision — surfaced for
// observability and deterministic snapshot tests.
type Decision struct {
	Mode           Mode
	Tools          []llm.ToolSpec
	ContextWindow  int
	Budget         int
	ToolTokens     int
	CompactTokens  int
	OverrideUsed   bool   // true when runtime.tool_injection forced the mode
	OverrideReason string // human-readable explanation
}

// Constants from the doc — kept here so a future cap revision is
// localised. Don't hardcode these anywhere else.
const (
	// MaxContextRatio is the fraction of the brain's context window
	// the planner is allowed to spend on tool schemas. From
	// docs-site/docs/language/04-tools.md _MAX_CONTEXT_RATIO.
	MaxContextRatio = 0.20

	// FallbackTokensPerTool is the per-tool estimate used when the
	// planner doesn't have direct_tools (i.e. before the index is
	// built). From _FALLBACK_TOKENS_PER_TOOL.
	FallbackTokensPerTool = 200

	// CompactBytesPerTool is the rough estimate for a compact-mode
	// listing (name + 1-line description). From the doc's
	// compact_tokens formula.
	CompactBytesPerTool = 30

	// DefaultContextWindow is the conservative fallback used when
	// neither the brain config nor the runtime config declares one.
	// Chosen to be small enough that an unconfigured app prefers
	// discovery over direct on a 50-tool catalogue ; protects
	// against runaway token cost on misconfigured deployments.
	DefaultContextWindow = 8000
)

// resolveContextWindow returns the effective context window for the
// given agent. Reads agent.brain.context.max_tokens first (since
// that's the doc-documented override path) ; falls back to the
// runtime.context.max_tokens block ; finally falls back to
// DefaultContextWindow.
//
// 0 / negative values are treated as "unset" and skipped.
func resolveContextWindow(agent *schema.Agent, runtime *schema.RuntimeBlock) int {
	if agent != nil && agent.Brain.Context != nil && agent.Brain.Context.MaxTokens > 0 {
		return agent.Brain.Context.MaxTokens
	}
	if runtime != nil && runtime.Context != nil && runtime.Context.MaxTokens > 0 {
		return runtime.Context.MaxTokens
	}
	return DefaultContextWindow
}

// resolveModeOverride returns (mode, true) when runtime.tool_injection
// is set ; otherwise (zero, false) signaling "use the algorithm".
func resolveModeOverride(rt *schema.RuntimeBlock) (Mode, bool) {
	if rt == nil || rt.ToolInjection == "" {
		return "", false
	}
	switch rt.ToolInjection {
	case schema.ToolInjectionDirect:
		return ModeDirect, true
	case schema.ToolInjectionCompactDirect:
		return ModeCompactDirect, true
	case schema.ToolInjectionDiscovery:
		return ModeDiscovery, true
	}
	// Unknown value : ignore (compiler validation should have caught it).
	return "", false
}
