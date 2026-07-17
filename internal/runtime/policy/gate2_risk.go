package policy

import (
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
)

func Gate2Risk(inv Invocation, pc PolicyContext) Decision {
	if !inv.Caller.IsLLM() {
		return allow(GateRisk)
	}
	if pc.ToolSpec == nil {
		return deny(GateRisk,
			"tool spec unavailable for "+inv.FQN()+" (cannot assess risk_level)")
	}

	ceiling := riskRank(deriveMaxRiskLevel(pc))
	actual := riskRank(pc.ToolSpec.RiskLevel)

	if actual >= 0 && ceiling >= 0 && actual <= ceiling {
		return allow(GateRisk)
	}

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

func deriveMaxRiskLevel(pc PolicyContext) tool.RiskLevel {
	if pc.Capabilities == nil || pc.Capabilities.MaxRiskLevel == "" {
		return tool.RiskMedium
	}
	return tool.RiskLevel(pc.Capabilities.MaxRiskLevel)
}

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
