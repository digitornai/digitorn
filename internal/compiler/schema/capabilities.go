package schema

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

type CapabilitiesConfig struct {
	DefaultPolicy   CapabilityPolicy  `yaml:"default_policy,omitempty"`
	MaxRiskLevel    RiskLevel         `yaml:"max_risk_level,omitempty"`
	Grant           []CapabilityGrant `yaml:"grant,omitempty"`
	Approve         []CapabilityGrant `yaml:"approve,omitempty"`
	Deny            []CapabilityGrant `yaml:"deny,omitempty"`
	ApprovalTimeout int               `yaml:"approval_timeout,omitempty"`
	HiddenModules   []string          `yaml:"hidden_modules,omitempty"`
	HiddenActions   []CapabilityGrant `yaml:"hidden_actions,omitempty"`

	// MaxDataClassification caps the sensitivity level an action may declare
	// (public | internal | confidential | restricted). Gate 5 blocks any
	// action whose data_classification exceeds this. Empty = no cap.
	MaxDataClassification string `yaml:"max_data_classification,omitempty"`

	// RateLimits caps calls per action over a sliding 60-second window.
	// Keys are "module.action" FQNs, or "*" for a default applied to every
	// action without an explicit key. Value is calls/minute (0 = unlimited).
	// Gate 6 blocks a call that would exceed its limit. Empty = no limiting.
	RateLimits map[string]int `yaml:"rate_limits,omitempty"`
}

type CapabilityGrant struct {
	Module  string   `yaml:"module"`
	Tools   []string `yaml:"tools,omitempty"`
	Actions []string `yaml:"actions,omitempty"` // deprecated alias of Tools
	Reason  string   `yaml:"reason,omitempty"`
}

func (g CapabilityGrant) EffectiveTools() []string {
	switch {
	case len(g.Actions) == 0:
		return g.Tools
	case len(g.Tools) == 0:
		return g.Actions
	default:
		out := make([]string, 0, len(g.Tools)+len(g.Actions))
		return append(append(out, g.Tools...), g.Actions...)
	}
}

// UnmarshalYAML accepts either the structured form {module, tools, reason}
// or the dotted shorthand "module.tool".
func (g *CapabilityGrant) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	switch node.Kind {
	case yaml.ScalarNode:
		parts := strings.SplitN(node.Value, ".", 2)
		g.Module = parts[0]
		if len(parts) == 2 && parts[1] != "" {
			g.Tools = []string{parts[1]}
		}
		return nil
	case yaml.MappingNode:
		type raw CapabilityGrant
		var r raw
		if err := node.Decode(&r); err != nil {
			return err
		}
		*g = CapabilityGrant(r)
		return nil
	default:
		return fmt.Errorf("capability grant: expected scalar or mapping, got kind %d", node.Kind)
	}
}
