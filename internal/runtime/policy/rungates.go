package policy

// SystemModules is the documented allow-list of modules that BYPASS
// every gate (security.md "System modules ... bypass the gates
// entirely - they're internal infrastructure, not user-facing
// tools"). Defined as a package-level set so the bypass check is
// O(1) on the hot path.
//
// The Python reference daemon hard-codes this list in security.py ;
// we mirror it verbatim. Adding a module here is intentional — it
// means the module is trusted infrastructure and not subject to
// agent-level capability policy.
var SystemModules = map[string]struct{}{
	"context_builder": {},
	"llm_provider":    {},
	"index":           {},
}

// MetaTools is the allow-list of context_builder meta-actions that
// bypass the gates at dispatcher level (security.md "The infrastructure
// meta-actions ... are also bypassed at the dispatcher level ; the gates
// apply to the target tool reached via execute_tool, not to the
// dispatcher itself"). The set mirrors the 10 builtins in
// injection.builtinToolSpecs : the 5 discovery meta-tools + the 5
// always-direct primitives. All of them are infrastructure, none is a
// user-facing tool, so security applies only to the sub-tool each one
// resolves.
//
// Matched by ACTION name regardless of module, so the bypass holds even
// when a primitive is reached by its bare name (no "context_builder."
// prefix) — the system-module bypass handles the prefixed form.
var MetaTools = map[string]struct{}{
	"search_tools":    {},
	"get_tool":        {},
	"execute_tool":    {},
	"list_categories": {},
	"browse_category": {},
	"run_parallel":    {},
	"background_run":  {},
	"use_skill":       {},
	"call_app":        {},
	"ask_user":        {},
	// agent : the delegation meta-tool, dispatched in-process by the
	// MetaDispatcher (no bus spec to assess risk against) and gated
	// separately by coordinator-role. Without this, reaching it by its
	// bare name (e.g. as a run_parallel / background_run child) trips
	// gate2_risk "tool spec unavailable" and is wrongly denied.
	"agent": {},
}

// RuntimeInternalModules are the modules implemented as RUNTIME SUBSYSTEMS
// (not service-bus modules) : the MetaDispatcher intercepts their actions and
// routes them to the engine directly (memory → MemoryWriter ; agent_spawn →
// AgentManager). They therefore have NO bus manifest and NO tool spec, so the
// spec-dependent gates (gate2_risk, gate3_permissions) cannot assess them and
// would fail closed. They never cross the service-bus boundary the gates guard,
// so — like the context_builder meta-tools — they bypass the gate chain. Their
// availability is controlled UPSTREAM, at injection time, by the documented
// module-declaration contract (tools.modules.memory / agent_spawn loaded) ;
// an undeclared module is never offered to the agent in the first place.
var RuntimeInternalModules = map[string]struct{}{
	"memory":      {},
	"agent_spawn": {},
}

// IsSystemModule returns true when the given module name is on the
// documented system-modules bypass list.
func IsSystemModule(module string) bool {
	_, ok := SystemModules[module]
	return ok
}

// IsRuntimeInternalModule returns true for modules implemented as runtime
// subsystems (memory, agent_spawn) whose actions are dispatcher-intercepted
// and never reach the service bus.
func IsRuntimeInternalModule(module string) bool {
	_, ok := RuntimeInternalModules[module]
	return ok
}

// IsMetaTool returns true when the given action name is on the
// documented meta-tools bypass list.
func IsMetaTool(action string) bool {
	_, ok := MetaTools[action]
	return ok
}

// RunGates is the orchestrator : it runs every implemented gate in
// the documented order (0 → 1a → 1b → 2 → 3 → 4) and returns the
// first non-Allow decision. If every gate Allows, returns Allow.
//
// System modules and meta-tools bypass everything — they short-circuit
// to Allow before the gate sequence runs. Per security.md :
//
//	"System modules (context_builder, llm_provider, index) bypass
//	 the gates entirely — they're internal infrastructure, not
//	 user-facing tools."
//	"The infrastructure meta-actions (execute_tool, search_tools, ...)
//	 are also bypassed at the dispatcher level ; the gates apply to
//	 the target tool reached via execute_tool, not to the dispatcher
//	 itself."
//
// The return value carries the GateCode that produced the decision
// so the audit row (SG-6) can be specific.
func RunGates(inv Invocation, pc PolicyContext) Decision {
	if IsSystemModule(inv.Module) {
		// "gate_bypass" style code so the audit row distinguishes
		// "explicitly allowed by system rule" from "passed every
		// gate". The forensic value is real : if you ever see a
		// system module's call in an unexpected place, the bypass
		// code tells you it was waved through without check.
		return Decision{Kind: DecisionAllow, Gate: "system_module_bypass",
			Reason: "module " + inv.Module + " is a trusted system module"}
	}
	if IsRuntimeInternalModule(inv.Module) {
		// memory.* / agent_spawn.agent are intercepted by the MetaDispatcher
		// and handled in-process (no bus spec to assess risk against). Their
		// availability is gated at injection by module declaration ; here they
		// short-circuit like the meta-tools so the spec gates don't fail closed.
		// Checked BEFORE the meta-tool bypass so the prefixed form keeps this
		// audit code (the bare "agent" falls through to meta_tool_bypass).
		return Decision{Kind: DecisionAllow, Gate: "runtime_internal_bypass",
			Reason: "module " + inv.Module + " is a runtime-internal subsystem (dispatcher-intercepted)"}
	}
	if IsMetaTool(inv.Action) {
		return Decision{Kind: DecisionAllow, Gate: "meta_tool_bypass",
			Reason: "action " + inv.Action + " is a meta-tool dispatched by the runtime"}
	}

	// Run the chain. First Deny or NeedsApproval wins ; Allow lets
	// the next gate run. If every gate Allows, the final Allow is
	// returned with the last gate's code (gate5_classification in the
	// normal flow).
	var last Decision
	for _, g := range gateChain {
		d := g(inv, pc)
		if d.IsBlocking() {
			return d
		}
		last = d
	}

	// Gate 6 (rate_limit) : STATEFUL, runtime-only. Applied here — never in
	// the pure gateChain — so the schema-build filter never consumes budget.
	// Runs LAST and only on an otherwise-allowed call, so a call denied by an
	// earlier gate doesn't count against the window. The limiter is nil on the
	// schema-build path (PolicyContext.RateLimiter unset), making gate 6 a
	// no-op there by construction.
	if pc.RateLimiter != nil {
		if reason := pc.RateLimiter.Check(inv.Module, inv.Action); reason != "" {
			return deny(GateRateLimit, reason)
		}
	}
	return last
}
