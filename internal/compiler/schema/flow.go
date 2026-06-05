package schema

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

type FlowConfig struct {
	ID            string     `yaml:"id,omitempty"`
	Description   string     `yaml:"description,omitempty"`
	Entry         string     `yaml:"entry,omitempty"`
	MaxIterations int        `yaml:"max_iterations,omitempty"`
	Nodes         []FlowNode `yaml:"nodes,omitempty"`
}

type FlowNode struct {
	ID          string           `yaml:"id"`
	Type        string           `yaml:"type"`
	Description string           `yaml:"description,omitempty"`
	Agent       string           `yaml:"agent,omitempty"`
	Tool        string           `yaml:"tool,omitempty"`
	Params      map[string]any   `yaml:"params,omitempty"`
	Branches    []FlowBranch     `yaml:"branches,omitempty"`
	Join        *FlowJoinConfig  `yaml:"join,omitempty"`
	Message     string           `yaml:"message,omitempty"`
	Choices     []any            `yaml:"choices,omitempty"`
	Expr        string           `yaml:"expr,omitempty"`
	Routes      []FlowRoute      `yaml:"routes,omitempty"`
	OnError     []FlowErrorRoute `yaml:"on_error,omitempty"`
	MaxIters    int              `yaml:"max_iterations,omitempty"`
}

// FlowBranch accepts either a bare node ID or {to: node_id}.
type FlowBranch struct {
	To string
}

func (b *FlowBranch) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	switch node.Kind {
	case yaml.ScalarNode:
		b.To = node.Value
		return nil
	case yaml.MappingNode:
		for i := 0; i < len(node.Content)-1; i += 2 {
			k, v := node.Content[i], node.Content[i+1]
			if k.Kind == yaml.ScalarNode && k.Value == "to" && v.Kind == yaml.ScalarNode {
				b.To = v.Value
				return nil
			}
		}
		return fmt.Errorf("flow branch: expected {to: <node>}")
	default:
		return fmt.Errorf("flow branch: expected scalar or mapping, got kind %d", node.Kind)
	}
}

type FlowJoinConfig struct {
	Type    string  `yaml:"type,omitempty"`
	Min     int     `yaml:"min,omitempty"`
	Timeout float64 `yaml:"timeout,omitempty"`
}

type FlowRoute struct {
	When    string `yaml:"when,omitempty"`
	Default bool   `yaml:"default,omitempty"`
	To      string `yaml:"to"`
}

type FlowErrorRoute struct {
	Match   string `yaml:"match,omitempty"`
	Default bool   `yaml:"default,omitempty"`
	To      string `yaml:"to"`
}
