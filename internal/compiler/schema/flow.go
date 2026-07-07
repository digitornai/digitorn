package schema

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

type FlowConfig struct {
	ID            string     `yaml:"id,omitempty" json:"id,omitempty"`
	Description   string     `yaml:"description,omitempty" json:"description,omitempty"`
	Entry         string     `yaml:"entry,omitempty" json:"entry,omitempty"`
	MaxIterations int        `yaml:"max_iterations,omitempty" json:"max_iterations,omitempty"`
	Nodes         []FlowNode `yaml:"nodes,omitempty" json:"nodes,omitempty"`
}

type FlowNode struct {
	ID          string           `yaml:"id" json:"id"`
	Type        string           `yaml:"type" json:"type"`
	Description string           `yaml:"description,omitempty" json:"description,omitempty"`
	Agent       string           `yaml:"agent,omitempty" json:"agent,omitempty"`
	Tool        string           `yaml:"tool,omitempty" json:"tool,omitempty"`
	Params      map[string]any   `yaml:"params,omitempty" json:"params,omitempty"`
	Branches    []FlowBranch     `yaml:"branches,omitempty" json:"branches,omitempty"`
	Join        *FlowJoinConfig  `yaml:"join,omitempty" json:"join,omitempty"`
	Message     string           `yaml:"message,omitempty" json:"message,omitempty"`
	Choices     []any            `yaml:"choices,omitempty" json:"choices,omitempty"`
	Expr        string           `yaml:"expr,omitempty" json:"expr,omitempty"`
	Routes      []FlowRoute      `yaml:"routes,omitempty" json:"routes,omitempty"`
	OnError     []FlowErrorRoute `yaml:"on_error,omitempty" json:"on_error,omitempty"`
	Retry       *FlowRetry       `yaml:"retry,omitempty" json:"retry,omitempty"`
	MaxIters    int              `yaml:"max_iterations,omitempty" json:"max_iterations,omitempty"`
}

// FlowRetry re-runs a failing node before its error routes fire, for transient
// faults (a rate-limited LLM, a flaky GLPI endpoint). Backoff grows
// geometrically: delay = BackoffMs * Multiplier^(attempt-1), capped at
// MaxBackoffMs. Match, when set, is a regexp the error must match to be retried
// (empty = retry any error). on_error still handles the final failure.
type FlowRetry struct {
	MaxAttempts  int     `yaml:"max_attempts,omitempty" json:"max_attempts,omitempty"`
	BackoffMs    int     `yaml:"backoff_ms,omitempty" json:"backoff_ms,omitempty"`
	Multiplier   float64 `yaml:"multiplier,omitempty" json:"multiplier,omitempty"`
	MaxBackoffMs int     `yaml:"max_backoff_ms,omitempty" json:"max_backoff_ms,omitempty"`
	Match        string  `yaml:"match,omitempty" json:"match,omitempty"`
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
	Type    string  `yaml:"type,omitempty" json:"type,omitempty"`
	Min     int     `yaml:"min,omitempty" json:"min,omitempty"`
	Timeout float64 `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

type FlowRoute struct {
	When    string `yaml:"when,omitempty" json:"when,omitempty"`
	Default bool   `yaml:"default,omitempty" json:"default,omitempty"`
	To      string `yaml:"to" json:"to"`
}

type FlowErrorRoute struct {
	Match   string `yaml:"match,omitempty" json:"match,omitempty"`
	Default bool   `yaml:"default,omitempty" json:"default,omitempty"`
	To      string `yaml:"to" json:"to"`
}
