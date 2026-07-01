package policy

import "github.com/digitornai/digitorn/internal/compiler/schema"

// Gate4Policy implements gate 4 of the documented security sequence
// — the policy resolver — as specified verbatim in
// docs-site/docs/language/11-security.md, section "Resolving a policy" :
//
//  1. Explicit deny in tools.capabilities.deny matching this
//     (module, action) pair → block.
//  2. Explicit approve in tools.capabilities.approve → approve (wait
//     for user OK).
//  3. Explicit grant in tools.capabilities.grant → auto (allowed,
//     no friction).
//  4. Per-grant default_action_policy — when a grant matches the
//     module without an explicit action policy, falls back to the
//     grant's own default_action_policy.
//  5. App-level default_policy — final fallback (approve by default).
//
// Step 4 (per-grant default_action_policy) is documented in the
// reference but has no YAML surface — schema.CapabilityGrant does
// not expose it. Implementing it now would be inventing a field that
// isn't in the doc ; we intentionally skip step 4 and fall straight
// from step 3 to step 5. If the field is added to the YAML later,
// step 4 plugs in cleanly between the existing branches.
//
// Caller-awareness (security-04-hidden-vs-deny.md "Callable from
// setup pipelines, hooks, channels" column) :
//
//   - `deny` is universal : it applies to every caller. The doc
//     repeatedly emphasises deny as "never legitimate in this app,
//     regardless of caller".
//   - `approve` is LLM-only : security-01-approval.md describes
//     approval as a synchronous pause of the agent loop. Hooks and
//     setup pipelines aren't an agent loop, so an approve hit while
//     they're calling falls through to the next step instead of
//     pausing.
//   - `grant` is universal : if a non-LLM caller hits a grant entry
//     it's still allowed (consistent with the spirit of grant).
//   - `default_policy`=approve is LLM-only ; for non-LLM callers it
//     degrades to allow.
//
// Returns one of Allow / Deny / NeedsApproval with code gate4_policy.
func Gate4Policy(inv Invocation, pc PolicyContext) Decision {
	caps := pc.Capabilities
	if caps == nil {
		// security.md : "Optional — absence means dev/test mode (no
		// enforcement)". Allow when no capabilities block.
		return allow(GatePolicy)
	}

	// Step 1 — explicit deny wins over everything, universal.
	for _, g := range caps.Deny {
		if matchesGrant(g, inv.Module, inv.Action) {
			return deny(GatePolicy, formatReason("denied", g))
		}
	}

	// Step 2 — explicit approve. LLM-only ; non-LLM callers don't
	// trigger an approval pause (no agent loop to suspend).
	if inv.Caller.IsLLM() {
		for _, g := range caps.Approve {
			if matchesGrant(g, inv.Module, inv.Action) {
				return needsApproval(GatePolicy, formatReason("approve", g))
			}
		}
	}

	// Step 3 — explicit grant. Universal.
	for _, g := range caps.Grant {
		if matchesGrant(g, inv.Module, inv.Action) {
			return allow(GatePolicy)
		}
	}

	// Step 5 — app-level default_policy. Step 4 (per-grant
	// default_action_policy) skipped — see comment at top of function.
	return resolveDefaultPolicy(caps.DefaultPolicy, inv)
}

// resolveDefaultPolicy applies the app-level default_policy. The
// doc default (security.md table) is "approve" when the field is
// empty. For non-LLM callers, "approve" degrades to allow because
// the approval flow only suspends the agent loop.
func resolveDefaultPolicy(p schema.CapabilityPolicy, inv Invocation) Decision {
	if p == "" {
		p = schema.CapApprove // doc default
	}
	switch p {
	case schema.CapAuto:
		return allow(GatePolicy)
	case schema.CapApprove:
		if inv.Caller.IsLLM() {
			return needsApproval(GatePolicy, "default policy requires approval")
		}
		return allow(GatePolicy) // non-LLM bypass
	case schema.CapBlock:
		return deny(GatePolicy, "default policy blocks this action")
	default:
		// Unknown policy value : fail-closed. The compiler validates
		// the enum at parse-time so we shouldn't normally land here ;
		// guard anyway for forward-compatibility.
		return deny(GatePolicy, "unknown default policy: "+string(p))
	}
}

// matchesGrant returns true when the grant entry covers the given
// (module, action). Empty Tools/Actions means "all actions of this
// module" — the documented convention for empty action lists in
// deny/approve/grant/hidden_actions.
func matchesGrant(g schema.CapabilityGrant, module, action string) bool {
	if !moduleMatches(g.Module, module) {
		return false
	}
	tools := g.EffectiveTools()
	if len(tools) == 0 {
		return true
	}
	for _, t := range tools {
		if t == action {
			return true
		}
	}
	return false
}

// formatReason builds a Reason string for the audit log. Uses the
// grant's Reason field when provided (CapabilityGrant.Reason in the
// doc is "human-readable, surfaced on deny events"), else falls
// back to a default phrasing keyed on the decision kind.
func formatReason(kind string, g schema.CapabilityGrant) string {
	if g.Reason != "" {
		return g.Reason
	}
	switch kind {
	case "denied":
		return "action explicitly denied by capabilities.deny"
	case "approve":
		return "action requires human approval (capabilities.approve)"
	default:
		return kind
	}
}
