package schema

import "gopkg.in/yaml.v3"

type ToolsBlock struct {
	Modules      map[string]ModuleBlock           `yaml:"modules,omitempty"`
	Capabilities *CapabilitiesConfig              `yaml:"capabilities,omitempty"`
	Channels     map[string]ChannelInstanceConfig `yaml:"channels,omitempty"`
}

type ModuleBlock struct {
	Config      map[string]any   `yaml:"config,omitempty"`
	Setup       []SetupStep      `yaml:"setup,omitempty"`
	Constraints map[string]any   `yaml:"constraints,omitempty"`
	Middleware  []map[string]any `yaml:"middleware,omitempty"`
	Credential  CredentialRef    `yaml:"credential,omitempty"`
}

var moduleBlockKnown = map[string]bool{
	"config": true, "setup": true, "constraints": true, "middleware": true, "credential": true,
}

// UnmarshalYAML decodes the known fields, then folds any remaining
// top-level keys into Config. This keeps backward compatibility with the
// flat module config of the old Python apps (tools.modules.rag: {backend:
// ..., embedding_model: ...}) while the Go convention nests under config:.
func (b *ModuleBlock) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	type alias ModuleBlock
	var a alias
	if err := node.Decode(&a); err != nil {
		return err
	}
	*b = ModuleBlock(a)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if moduleBlockKnown[key] {
			continue
		}
		var val any
		if err := node.Content[i+1].Decode(&val); err != nil {
			return err
		}
		if b.Config == nil {
			b.Config = map[string]any{}
		}
		if _, exists := b.Config[key]; !exists {
			b.Config[key] = val
		}
	}
	return nil
}

type SetupStep struct {
	Action string         `yaml:"action"`
	Params map[string]any `yaml:"params,omitempty"`
}
