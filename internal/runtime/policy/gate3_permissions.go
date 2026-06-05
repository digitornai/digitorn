package policy

import "github.com/mbathepaul/digitorn/internal/compiler/schema"

// Gate3Permissions implements gate 3 of the documented security
// sequence (docs-site/docs/tutorial/security-02-gates.md, line 84) :
//
//	"Some actions declare symbolic required_permissions (fs.write,
//	 net.http, process.spawn). The agent's profile must grant the
//	 matching permission set. Granular alternative to listing every
//	 action by name."
//
// The doc describes the concept but provides NO YAML surface for an
// app dev to declare "the agent's granted permissions". Audit of the
// reference daemon's security.py confirmed the resolution model :
//
//   - The "granted permissions" set is DERIVED automatically from
//     capabilities.grant entries, in the form "{module}:{action}".
//     Source : compiler.py::_build_security_profile lines 3193-3398
//     (sole populator of SecurityProfile.granted_permissions).
//
//   - Gate 3 is structured as two checks (security.py:337-350) :
//     1. action_granted shortcut : if "{module}:{action}" is in the
//     granted set, gate 3 passes regardless of required_permissions.
//     2. otherwise : every entry in required_permissions must match
//     an entry in the granted set (Python uses fnmatch glob ; we
//     use exact equality — fnmatch never triggers in practice
//     because granted entries are always "module:action" strings
//     and required entries are typically "fs.write" style).
//
// Net effect : gate 3 is a secondary check that only denies an action
// when (a) default_policy would have let it through, (b) the action
// declares required_permissions, and (c) those permissions don't
// coincidentally match a granted "module:action" string. This mirrors
// the Python daemon's actual runtime behaviour bit-for-bit, with
// ZERO YAML extensions.
//
// LLM-specific gate : bypasses for hooks/setup/channels.
func Gate3Permissions(inv Invocation, pc PolicyContext) Decision {
	if !inv.Caller.IsLLM() {
		return allow(GatePermissions)
	}
	if pc.ToolSpec == nil {
		// No spec means we don't know what permissions are required.
		// Fail-closed (consistent with gate 2 stance).
		return deny(GatePermissions,
			"tool spec unavailable for "+inv.FQN()+" (cannot assess required_permissions)")
	}
	if len(pc.ToolSpec.Permissions) == 0 {
		// Action declares no symbolic permission — gate 3 is a no-op
		// for this action.
		return allow(GatePermissions)
	}

	// Check 1 : action_granted shortcut. If the action is an explicit
	// operator opt-in — grant OR approve — gate 3 passes without inspecting
	// required_permissions. approve must count here exactly like grant : it is
	// a deliberate inclusion of the action, just one whose execution is gated
	// by human confirmation at gate 4. Without this, an approve-listed action
	// (e.g. bash.run, which declares Permissions:["bash.run"]) falls through to
	// the symbolic-permission check, finds its permission ungranted, and is
	// silently filtered out of the toolset — so the agent reports "no shell".
	if pc.Capabilities != nil && hasExplicitCapability(pc.Capabilities, inv.Module, inv.Action) {
		return allow(GatePermissions)
	}

	// Check 2 : require every required_permission to appear in the
	// derived granted set. Granted entries are "module:action"
	// strings ; this rarely matches "fs.write" style required
	// permissions, which is consistent with the Python daemon's
	// behaviour where gate 3 effectively becomes a strict denial
	// for symbolic-required actions that aren't explicitly granted.
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

// derivedGrantedPermissions builds the set Python mirrors in
// SecurityProfile.granted_permissions — every (module, action) pair
// covered by an explicit capabilities.grant entry, serialised as
// "module:action".
//
// Grant entries with empty actions list (= "all actions of this
// module") cannot be enumerated without the module manifest, so
// those don't produce permission strings ; gates 1a/4 cover the
// broad-allow case independently.
//
// Returns nil when no grants are configured.
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
	// Both grant and approve are deliberate opt-ins, so both contribute their
	// "module:action" permission strings ; approve just adds a runtime human
	// gate (gate 4), it does not change what the action is permitted to be.
	add(caps.Grant)
	add(caps.Approve)
	return set
}
