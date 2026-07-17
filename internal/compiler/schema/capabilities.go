package schema

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

type CapabilitiesConfig struct {
	DefaultPolicy   CapabilityPolicy  `yaml:"default_policy,omitempty" json:"default_policy,omitempty"`
	MaxRiskLevel    RiskLevel         `yaml:"max_risk_level,omitempty" json:"max_risk_level,omitempty"`
	Grant           []CapabilityGrant `yaml:"grant,omitempty" json:"grant,omitempty"`
	Approve         []CapabilityGrant `yaml:"approve,omitempty" json:"approve,omitempty"`
	Deny            []CapabilityGrant `yaml:"deny,omitempty" json:"deny,omitempty"`
	ApprovalTimeout int               `yaml:"approval_timeout,omitempty" json:"approval_timeout,omitempty"`
	HiddenModules   []string          `yaml:"hidden_modules,omitempty" json:"hidden_modules,omitempty"`
	HiddenActions   []CapabilityGrant `yaml:"hidden_actions,omitempty" json:"hidden_actions,omitempty"`

	MaxDataClassification string `yaml:"max_data_classification,omitempty" json:"max_data_classification,omitempty"`

	RateLimits map[string]int `yaml:"rate_limits,omitempty" json:"rate_limits,omitempty"`
}

type CapabilityGrant struct {
	Module  string   `yaml:"module" json:"module"`
	Tools   []string `yaml:"tools,omitempty" json:"tools,omitempty"`
	Actions []string `yaml:"actions,omitempty" json:"actions,omitempty"`
	Reason  string   `yaml:"reason,omitempty" json:"reason,omitempty"`
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
