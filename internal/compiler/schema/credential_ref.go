package schema

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// CredentialRef accepts either "my_secret" or {ref, scope, provider}.
type CredentialRef struct {
	Ref      string
	Scope    CredentialScope
	Provider string
}

func (c CredentialRef) IsSet() bool { return c.Ref != "" }

func (c *CredentialRef) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	switch node.Kind {
	case yaml.ScalarNode:
		c.Ref = node.Value
		return nil
	case yaml.MappingNode:
		for i := 0; i < len(node.Content)-1; i += 2 {
			k, v := node.Content[i], node.Content[i+1]
			if k.Kind != yaml.ScalarNode || v.Kind != yaml.ScalarNode {
				continue
			}
			switch k.Value {
			case "ref":
				c.Ref = v.Value
			case "scope":
				c.Scope = CredentialScope(v.Value)
			case "provider":
				c.Provider = v.Value
			default:
				return fmt.Errorf("credential: unknown field %q (allowed: ref, scope, provider)", k.Value)
			}
		}
		return nil
	default:
		return fmt.Errorf("credential: expected scalar or mapping, got kind %d", node.Kind)
	}
}
