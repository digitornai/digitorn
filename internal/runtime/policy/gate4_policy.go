package policy

import "github.com/digitornai/digitorn/internal/compiler/schema"

func Gate4Policy(inv Invocation, pc PolicyContext) Decision {
	caps := pc.Capabilities
	if caps == nil {
		return allow(GatePolicy)
	}

	for _, g := range caps.Deny {
		if matchesGrant(g, inv.Module, inv.Action) {
			return deny(GatePolicy, formatReason("denied", g))
		}
	}

	if inv.Caller.IsLLM() {
		for _, g := range caps.Approve {
			if matchesGrant(g, inv.Module, inv.Action) {
				return needsApproval(GatePolicy, formatReason("approve", g))
			}
		}
	}

	for _, g := range caps.Grant {
		if matchesGrant(g, inv.Module, inv.Action) {
			return allow(GatePolicy)
		}
	}

	return resolveDefaultPolicy(caps.DefaultPolicy, inv)
}

func resolveDefaultPolicy(p schema.CapabilityPolicy, inv Invocation) Decision {
	if p == "" {
		p = schema.CapApprove
	}
	switch p {
	case schema.CapAuto:
		return allow(GatePolicy)
	case schema.CapApprove:
		if inv.Caller.IsLLM() {
			return needsApproval(GatePolicy, "default policy requires approval")
		}
		return allow(GatePolicy)
	case schema.CapBlock:
		return deny(GatePolicy, "default policy blocks this action")
	default:
		return deny(GatePolicy, "unknown default policy: "+string(p))
	}
}

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
