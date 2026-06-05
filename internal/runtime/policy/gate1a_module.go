package policy

// Gate1aModule implements gate 1a of the documented security
// sequence (docs-site/docs/tutorial/security-02-gates.md, line 68) :
//
//	"The agent's profile lists which modules it can call. If the
//	 action's module isn't there, the call is blocked. This is what
//	 per-agent module restriction (advanced-01-sub-agent-isolation.md)
//	 relies on."
//
// LLM-specific gate : bypasses for hooks, setup pipelines and
// channel callers. Those callers operate at a layer below the
// agent and aren't bound by the agent's module subset.
//
// Returns Deny with code gate1a_module when the agent's resolved
// module list does not include this (module, action). Allow when
// allowed.
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
