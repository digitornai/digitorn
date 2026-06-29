package schema

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// UnmarshalYAML accepts the three documented forms for a runtime.middleware
// entry, normalising all to {Name, Enabled, Config}:
//
//   - mask_secrets                         # bare string
//   - mask_secrets: { patterns: [...] }    # name-as-key (+ optional enabled)
//   - { name: mask_secrets, config: {...}, enabled: true }   # structured
func (e *MiddlewareEntry) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		e.Name = node.Value
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("middleware entry must be a string or a mapping")
	}

	hasName := false
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "name" {
			hasName = true
			break
		}
	}

	if hasName {
		type raw struct {
			Name    string         `yaml:"name" json:"name"`
			Enabled *bool          `yaml:"enabled" json:"enabled"`
			Config  map[string]any `yaml:"config" json:"config"`
		}
		var r raw
		if err := node.Decode(&r); err != nil {
			return err
		}
		e.Name, e.Enabled, e.Config = r.Name, r.Enabled, r.Config
		return nil
	}

	// name-as-key : the single key is the middleware name, its value is the
	// config block. An `enabled` key inside that block is lifted out so the
	// disable toggle works in this form too.
	if len(node.Content) < 2 {
		return fmt.Errorf("middleware entry mapping is empty")
	}
	e.Name = node.Content[0].Value
	if v := node.Content[1]; v.Kind == yaml.MappingNode {
		cfg := map[string]any{}
		if err := v.Decode(&cfg); err != nil {
			return err
		}
		if en, ok := cfg["enabled"].(bool); ok {
			e.Enabled = &en
			delete(cfg, "enabled")
		}
		e.Config = cfg
	}
	return nil
}
