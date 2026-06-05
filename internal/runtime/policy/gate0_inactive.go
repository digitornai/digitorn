package policy

// Gate0Inactive implements gate 0 of the documented security
// sequence (docs-site/docs/tutorial/security-02-gates.md, line 62) :
//
//	"A trivial check that the app is deployed and the agent is allowed
//	 to run at all. Admin profiles bypass it. Useful for putting an
//	 app into a 'soft-undeploy' state without deleting its bundle."
//
// Universal — runs for every caller (LLM, hook, setup, channel).
// An inactive app must not run anything, regardless of who's calling.
//
// Returns Deny with code gate0_inactive when the app is disabled.
// Allow when active.
func Gate0Inactive(_ Invocation, pc PolicyContext) Decision {
	if !pc.AppActive {
		return deny(GateInactive, "app is not active (deployed but disabled)")
	}
	return allow(GateInactive)
}
