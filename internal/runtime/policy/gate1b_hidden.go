package policy

func Gate1bHidden(inv Invocation, pc PolicyContext) Decision {
	if !inv.Caller.IsLLM() {
		return allow(GateHidden)
	}
	if pc.Capabilities == nil {
		return allow(GateHidden)
	}
	caps := pc.Capabilities

	for _, m := range caps.HiddenModules {
		if moduleMatches(m, inv.Module) {
			return deny(GateHidden,
				"module "+inv.Module+" is in hidden_modules (LLM-invisible)")
		}
	}

	for _, g := range caps.HiddenActions {
		if !moduleMatches(g.Module, inv.Module) {
			continue
		}
		tools := g.EffectiveTools()
		if len(tools) == 0 {
			return deny(GateHidden,
				"module "+inv.Module+" has all actions hidden via hidden_actions")
		}
		for _, t := range tools {
			if t == inv.Action {
				return deny(GateHidden,
					"action "+inv.FQN()+" is in hidden_actions (LLM-invisible)")
			}
		}
	}
	return allow(GateHidden)
}
