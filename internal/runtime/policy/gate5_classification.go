package policy

import "strings"

// classificationRank orders data-classification levels low→high. Matches the
// reference daemon's _CLASSIFICATION_RANK (security_enforcer.py).
var classificationRank = map[string]int{
	"public":       0,
	"internal":     1,
	"confidential": 2,
	"restricted":   3,
}

// Gate5Classification denies an action whose declared data_classification
// exceeds the app's max_data_classification. Pure — it lives in the shared
// gateChain so it filters over-classified tools at schema-build AND blocks
// them at runtime. No-op when either side is unset (an unclassified action,
// or no cap configured), so it never fires for apps that don't opt in.
//
// Unknown classification strings default the action to "internal" (1) and the
// cap to "restricted" (3), matching the reference defaults — an unrecognised
// level never accidentally tightens or loosens the gate beyond those bounds.
func Gate5Classification(_ Invocation, pc PolicyContext) Decision {
	if pc.ToolSpec == nil || pc.Capabilities == nil {
		return allow(GateClassification)
	}
	action := strings.ToLower(strings.TrimSpace(pc.ToolSpec.DataClassification))
	max := strings.ToLower(strings.TrimSpace(pc.Capabilities.MaxDataClassification))
	if action == "" || max == "" {
		return allow(GateClassification)
	}
	actionRank, ok := classificationRank[action]
	if !ok {
		actionRank = 1
	}
	maxRank, ok := classificationRank[max]
	if !ok {
		maxRank = 3
	}
	if actionRank > maxRank {
		return deny(GateClassification,
			"data classification '"+action+"' exceeds maximum '"+max+"' for this application")
	}
	return allow(GateClassification)
}
