package policy

// Gate1bHidden implements gate 1b of the documented security
// sequence (docs-site/docs/tutorial/security-02-gates.md, line 73) :
//
//	"tools.capabilities.hidden_actions lists actions the agent CANNOT
//	 see in its tool index. Different from deny: a hidden action can
//	 still be called from setup steps, hooks, or channel pipelines ;
//	 it's only invisible to the LLM."
//
// Crucial difference with deny, documented in security-04-hidden-vs-deny.md :
//
//	"`hidden` is a declutter mechanism: stop showing the LLM options
//	 it shouldn't reach for, but keep them callable from your
//	 infrastructure."
//
// LLM-specific gate : bypasses for hooks, setup pipelines and
// channels. Those can call hidden actions deliberately — that's the
// whole point of hidden vs deny.
//
// Two sources of hiding :
//
//   - HiddenModules : every action of these modules is hidden
//   - HiddenActions : per-(module, action) entries, same shape as
//     deny/grant/approve (security-04-hidden-vs-deny.md YAML example)
//
// Returns Deny with code gate1b_hidden when the action is hidden
// from the LLM. Allow otherwise.
func Gate1bHidden(inv Invocation, pc PolicyContext) Decision {
	if !inv.Caller.IsLLM() {
		return allow(GateHidden)
	}
	if pc.Capabilities == nil {
		return allow(GateHidden)
	}
	caps := pc.Capabilities

	// Whole-module hidden ?
	for _, m := range caps.HiddenModules {
		if m == inv.Module {
			return deny(GateHidden,
				"module "+inv.Module+" is in hidden_modules (LLM-invisible)")
		}
	}

	// Per-(module, action) hidden ?
	for _, g := range caps.HiddenActions {
		if g.Module != inv.Module {
			continue
		}
		tools := g.EffectiveTools()
		// Empty tools list = "all actions of this module" — same
		// convention as grant/deny entries.
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
