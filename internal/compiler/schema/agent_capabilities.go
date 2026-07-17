package schema

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

type AgentCapabilities struct {
	Skills []string
	Config *CapabilitiesConfig
}

func (c *AgentCapabilities) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	switch node.Kind {
	case yaml.SequenceNode:
		for _, it := range node.Content {
			if it.Kind == yaml.ScalarNode {
				c.Skills = append(c.Skills, it.Value)
			}
		}
		return nil
	case yaml.MappingNode:
		var cfg CapabilitiesConfig
		if err := node.Decode(&cfg); err != nil {
			return err
		}
		c.Config = &cfg
		return nil
	default:
		return fmt.Errorf("agent capabilities: expected sequence or mapping, got kind %d", node.Kind)
	}
}
