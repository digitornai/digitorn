package policy

import (
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
)

// BuildAgentToolset is the documented PRIMARY defence layer of the
// security model. From security-02-gates.md :
//
//	"This is more secure than runtime rejection. A schema filter
//	 denies the model the choice in the first place."
//
// It walks the universe of (module, action) pairs available to the
// app, runs gates 0/1a/1b/2/3/4 against each with Caller=LLM, and
// returns only the ones the LLM is allowed to see.
//
// Decisions are mapped to inclusion :
//
//   - Allow          → included
//   - NeedsApproval  → included (the LLM may attempt the call ; the
//     approval pause is triggered at runtime by
//     gate 4 in SG-4)
//   - Deny           → excluded
//
// The function is pure : no I/O, no logging. The caller (SG-4 wirage
// in engine.runPhases) is responsible for emitting an
// EventSecurityDecision per filtered-out action (SG-6) and for
// converting the resulting list into llm.ToolSpec for the chat
// request.
//
// AvailableAction is one input row : a (module, action, spec) tuple.
// SG-4 will populate this slice by walking the module dispatcher's
// catalog once per app version.
type AvailableAction struct {
	Module string
	Action string
	Spec   *tool.Spec
}

// BuildAgentToolset returns the subset of `actions` the agent is
// allowed to see in its LLM tool list. Order is preserved : the
// dispatcher catalog ordering is the LLM's tool ordering.
//
// Inputs :
//
//   - appActive    : appmgr.App.Enabled, drives gate 0
//   - caps         : the app-level tools.capabilities block. nil =
//     dev/test mode (no enforcement, every action
//     is included).
//   - agent        : the specific agent we're building the toolset
//     for. The agent's modules subset (gate 1a) and
//     permissions (gate 3) come from here.
//   - actions      : all known (module, action, spec) tuples — the
//     universe to filter from.
//
// Returns a NEW slice ; the input is not mutated.
func BuildAgentToolset(
	appActive bool,
	caps *schema.CapabilitiesConfig,
	agent *schema.Agent,
	actions []AvailableAction,
) []AvailableAction {
	// Cache the resolved agent-modules lookup once per call. The
	// per-action gate1a then runs in O(1) instead of O(len(modules)).
	var agentModules map[string]AgentModuleAccess
	if agent != nil {
		agentModules = ResolveAgentModules(agent.Modules)
	}

	out := make([]AvailableAction, 0, len(actions))
	for _, a := range actions {
		inv := Invocation{
			Caller: CallerLLM,
			Module: a.Module,
			Action: a.Action,
		}
		pc := PolicyContext{
			AppActive:    appActive,
			Capabilities: caps,
			AgentModules: agentModules,
			ToolSpec:     a.Spec,
		}
		if !passesAllGates(inv, pc) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// passesAllGates runs the pure gates (0, 1a, 1b, 2, 3, 4, 5) in
// documented order and returns true when NO gate returned Deny. It runs
// every gate (it does not stop on NeedsApproval), so an over-classified
// tool is filtered even when its policy is "approve". NeedsApproval at
// gate 4 is treated as "passes" — the LLM keeps the tool in its index,
// the approval pause fires at runtime. Gate 6 (rate_limit) is stateful
// and never runs here — it is a runtime-only concern.
//
// Order is fixed by the doc (security-02-gates.md "The sequence"
// diagram) ; do NOT reorder these calls.
func passesAllGates(inv Invocation, pc PolicyContext) bool {
	for _, g := range gateChain {
		d := g(inv, pc)
		if d.Kind == DecisionDeny {
			return false
		}
	}
	return true
}

// gateChain is the documented gate order, in the slice the schema-build
// filter (passesAllGates) and the runtime evaluator (RunGates) both walk.
// Defined at package scope so the slice isn't reallocated per action.
//
// Gate 5 (data classification) is PURE and lives here, so it filters
// over-classified tools at schema-build AND blocks them at runtime, just
// like gate 2. Gate 6 (rate_limit) is STATEFUL and runtime-only — it is
// NOT in this chain ; RunGates applies it separately so building the tool
// list never consumes rate budget.
var gateChain = []func(Invocation, PolicyContext) Decision{
	Gate0Inactive,
	Gate1aModule,
	Gate1bHidden,
	Gate2Risk,
	Gate3Permissions,
	Gate4Policy,
	Gate5Classification,
}

// ResolveAgentModules converts the YAML-shaped schema.AgentModules
// into the lookup map gate1a expects. Three documented YAML shapes
// produce the same in-memory form :
//
//   - modules: [filesystem, shell]
//     → both modules with AllActions=true
//   - modules: [{filesystem: [read, glob, grep]}]
//     → filesystem with Actions={read,glob,grep}
//   - modules: {filesystem: [read], shell: [bash]}
//     → both modules with their action subsets
//
// Returns nil when the agent declares no modules — that means "no
// restriction at the agent level", and gate1a will accept any module
// the app capabilities allow.
//
// Exported so SG-4 (and tests) can build a PolicyContext without
// going through the whole BuildAgentToolset machinery.
func ResolveAgentModules(mods schema.AgentModules) map[string]AgentModuleAccess {
	if len(mods) == 0 {
		return nil
	}
	out := make(map[string]AgentModuleAccess, len(mods))
	for _, ref := range mods {
		if ref.ID == "" {
			continue // defensive : shouldn't happen post-parse
		}
		access := out[ref.ID]
		if len(ref.Tools) == 0 {
			// Bare module name in YAML = all actions.
			access.AllActions = true
		} else {
			if access.Actions == nil {
				access.Actions = make(map[string]struct{}, len(ref.Tools))
			}
			for _, t := range ref.Tools {
				access.Actions[t] = struct{}{}
			}
		}
		out[ref.ID] = access
	}
	return out
}
