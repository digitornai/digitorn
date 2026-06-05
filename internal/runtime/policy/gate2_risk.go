package policy

import (
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
)

// Gate2Risk implements gate 2 of the documented security sequence
// (docs-site/docs/tutorial/security-02-gates.md, line 79) :
//
//	"Every @action declares a risk_level: low | medium | high.
//	 max_risk_level caps the ceiling. Actions above the ceiling are
//	 filtered out at schema-build time, before the LLM ever sees them."
//
// The doc emphasises why : "this is more secure than runtime
// rejection. A schema filter denies the model the choice in the
// first place." Same function feeds both schema-build filter (SG-3)
// and runtime check (SG-4).
//
// LLM-specific gate : bypasses for hooks, setup pipelines and
// channels. The doc implies (security-02-gates.md "Why gate 2") that
// the risk ceiling is about what the LLM is allowed to TRY, not
// about hard prohibition — hard prohibition is gate 4 deny.
//
// Levels are ordered : low < medium < high. The reference doc lists
// the levels in `language/11-security.md`. We compare against the
// app's max_risk_level. If max_risk_level is empty, the doc says
// the default is "medium" — we mirror that here so unconfigured
// apps still cap high-risk actions.
//
// IMPORTANT — explicit grant bypasses the cap. The doc
// (language/04-tools.md "max_risk_level" example) says verbatim :
//
//	max_risk_level: low               # only "low" actions auto-allowed
//	grant:
//	  - { module: shell, actions: [bash] }   # explicit grant bypasses the cap
//
// So when an action's risk exceeds the ceiling BUT the action is
// listed in capabilities.grant, gate 2 must allow. Gate 4 then
// still applies its own deny check, so a contradictory
// {grant + deny} on the same action still denies overall (via
// gate 4) — consistent with doc precedence "deny > grant".
//
// The same applies to capabilities.approve : a high-risk tool placed
// under `approve` is a deliberate opt-in meant to be offered and gated
// by human confirmation. It must bypass the ceiling too, or the safer
// policy (approve) would paradoxically hide the tool while the riskier
// one (grant) keeps it. See hasExplicitCapability.
//
// Returns Deny with code gate2_risk when the action's level exceeds
// the ceiling AND no explicit grant overrides it. Allow otherwise.
func Gate2Risk(inv Invocation, pc PolicyContext) Decision {
	if !inv.Caller.IsLLM() {
		return allow(GateRisk)
	}
	if pc.ToolSpec == nil {
		// No spec means we cannot reason about risk. Default to
		// deny — the doc's spirit is to fail-closed on unknowns.
		return deny(GateRisk,
			"tool spec unavailable for "+inv.FQN()+" (cannot assess risk_level)")
	}

	ceiling := riskRank(deriveMaxRiskLevel(pc))
	actual := riskRank(pc.ToolSpec.RiskLevel)

	// Fail-CLOSED on anything we cannot rank. riskRank returns -1 for an
	// empty / unknown / typo'd level, and a naive `actual <= ceiling` would let
	// such an action (actual=-1) slip UNDER any ceiling — a silent bypass of
	// max_risk_level. So we allow ONLY when both ranks are known AND the action
	// is at or below the ceiling. An unrankable action OR a misconfigured
	// ceiling falls through to the grant/deny path below.
	if actual >= 0 && ceiling >= 0 && actual <= ceiling {
		return allow(GateRisk)
	}

	// Not auto-allowed (over the ceiling, or unrankable). An explicit operator
	// opt-in — grant OR approve — still bypasses the cap ; gate 4 then governs
	// (grant → allow, approve → NeedsApproval). Crucially `approve` must bypass
	// too : it is the SAFER opt-in (the human confirms each call), so it would
	// be backwards to keep a granted high-risk tool while silently removing the
	// approval-gated one. The doc's "grant bypasses the cap" example predates
	// approve ; the gate's own contract — the ceiling bounds what the LLM may
	// TRY, hard prohibition is gate 4 deny — applies identically to approve.
	if pc.Capabilities != nil && hasExplicitCapability(pc.Capabilities, inv.Module, inv.Action) {
		return allow(GateRisk)
	}
	if actual < 0 {
		return deny(GateRisk,
			"action "+inv.FQN()+" has unrecognised risk_level="+string(pc.ToolSpec.RiskLevel)+
				" — cannot assess (fail-closed)")
	}
	if ceiling < 0 {
		return deny(GateRisk,
			"app max_risk_level="+string(deriveMaxRiskLevel(pc))+
				" is unrecognised — cannot assess (fail-closed)")
	}
	return deny(GateRisk,
		"action "+inv.FQN()+" risk_level="+string(pc.ToolSpec.RiskLevel)+
			" exceeds max_risk_level="+string(deriveMaxRiskLevel(pc)))
}

// hasExplicitCapability reports whether (module, action) is an explicit
// operator opt-in — listed in capabilities.grant OR capabilities.approve.
// Used by gate 2 to honour the "explicit opt-in bypasses the risk cap" rule :
// a deliberately-listed tool reaches gate 4 (grant → allow, approve →
// NeedsApproval) rather than being silently filtered out by the ceiling.
// capabilities.deny is NOT an opt-in (it's an opt-OUT, enforced by gate 4).
func hasExplicitCapability(caps *schema.CapabilitiesConfig, module, action string) bool {
	for _, g := range caps.Grant {
		if matchesGrant(g, module, action) {
			return true
		}
	}
	for _, g := range caps.Approve {
		if matchesGrant(g, module, action) {
			return true
		}
	}
	return false
}

// deriveMaxRiskLevel returns the effective ceiling. Doc default
// (security-02-gates.md "max_risk_level: medium" example + the
// "default medium" mention in the language reference) is medium when
// the field is unset.
func deriveMaxRiskLevel(pc PolicyContext) tool.RiskLevel {
	if pc.Capabilities == nil || pc.Capabilities.MaxRiskLevel == "" {
		return tool.RiskMedium
	}
	return tool.RiskLevel(pc.Capabilities.MaxRiskLevel)
}

// riskRank converts a RiskLevel string to an integer ranking so a
// simple comparison decides the gate. Unknown levels rank as -1 so
// any comparison with a known ceiling rejects them — fail-closed.
func riskRank(r tool.RiskLevel) int {
	switch r {
	case tool.RiskLow:
		return 0
	case tool.RiskMedium:
		return 1
	case tool.RiskHigh:
		return 2
	default:
		return -1
	}
}
