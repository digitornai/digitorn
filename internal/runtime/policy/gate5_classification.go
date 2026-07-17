package policy

import "strings"

var classificationRank = map[string]int{
	"public":       0,
	"internal":     1,
	"confidential": 2,
	"restricted":   3,
}

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
