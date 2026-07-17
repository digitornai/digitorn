package policy

var SystemModules = map[string]struct{}{
	"context_builder": {},
	"llm_provider":    {},
	"index":           {},
}

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
	"agent": {},
}

var RuntimeInternalModules = map[string]struct{}{
	"memory":      {},
	"agent_spawn": {},
}

func IsSystemModule(module string) bool {
	_, ok := SystemModules[module]
	return ok
}

func IsRuntimeInternalModule(module string) bool {
	_, ok := RuntimeInternalModules[module]
	return ok
}

func IsMetaTool(action string) bool {
	_, ok := MetaTools[action]
	return ok
}

func RunGates(inv Invocation, pc PolicyContext) Decision {
	if IsSystemModule(inv.Module) {
		return Decision{Kind: DecisionAllow, Gate: "system_module_bypass",
			Reason: "module " + inv.Module + " is a trusted system module"}
	}
	if IsRuntimeInternalModule(inv.Module) {
		return Decision{Kind: DecisionAllow, Gate: "runtime_internal_bypass",
			Reason: "module " + inv.Module + " is a runtime-internal subsystem (dispatcher-intercepted)"}
	}
	if IsMetaTool(inv.Action) {
		return Decision{Kind: DecisionAllow, Gate: "meta_tool_bypass",
			Reason: "action " + inv.Action + " is a meta-tool dispatched by the runtime"}
	}

	var last Decision
	for _, g := range gateChain {
		d := g(inv, pc)
		if d.IsBlocking() {
			return d
		}
		last = d
	}

	if pc.RateLimiter != nil {
		if reason := pc.RateLimiter.Check(inv.Module, inv.Action); reason != "" {
			return deny(GateRateLimit, reason)
		}
	}
	return last
}
