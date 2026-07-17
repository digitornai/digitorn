package policy

func Gate1aModule(inv Invocation, pc PolicyContext) Decision {
	if !inv.Caller.IsLLM() {
		return allow(GateModule)
	}
	if !pc.CanAgentCall(inv.Module, inv.Action) {
		return deny(GateModule,
			"agent profile does not grant access to module "+inv.Module+
				" (or action "+inv.Action+" not in the agent's subset)")
	}
	return allow(GateModule)
}
