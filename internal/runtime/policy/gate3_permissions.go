package policy

import "github.com/digitornai/digitorn/internal/compiler/schema"

func Gate3Permissions(inv Invocation, pc PolicyContext) Decision {
	if !inv.Caller.IsLLM() {
		return allow(GatePermissions)
	}
	if pc.ToolSpec == nil {
		return deny(GatePermissions,
			"tool spec unavailable for "+inv.FQN()+" (cannot assess required_permissions)")
	}
	if len(pc.ToolSpec.Permissions) == 0 {
		return allow(GatePermissions)
	}

	if pc.Capabilities != nil && hasExplicitCapability(pc.Capabilities, inv.Module, inv.Action) {
		return allow(GatePermissions)
	}

	granted := derivedGrantedPermissions(pc.Capabilities)
	for _, req := range pc.ToolSpec.Permissions {
		if _, ok := granted[req]; !ok {
			return deny(GatePermissions,
				"required permission \""+req+"\" not in the agent's granted set "+
					"for action "+inv.FQN())
		}
	}
	return allow(GatePermissions)
}

func derivedGrantedPermissions(caps *schema.CapabilitiesConfig) map[string]struct{} {
	if caps == nil || (len(caps.Grant) == 0 && len(caps.Approve) == 0) {
		return nil
	}
	set := make(map[string]struct{})
	add := func(entries []schema.CapabilityGrant) {
		for _, g := range entries {
			tools := g.EffectiveTools()
			if len(tools) == 0 {
				continue
			}
			for _, t := range tools {
				set[g.Module+":"+t] = struct{}{}
			}
		}
	}
	add(caps.Grant)
	add(caps.Approve)
	return set
}
