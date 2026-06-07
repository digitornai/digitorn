package schema

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

type Agent struct {
	ID           string             `yaml:"id"`
	Role         string             `yaml:"role,omitempty"`
	Brain        Brain              `yaml:"brain"`
	// MaxToolIterations caps the LLM↔tool rounds in a single turn for this agent.
	// 0/unset → the engine's (high) default. Set it higher for long agentic tasks
	// (scaffold + install + run), lower to keep a turn short.
	MaxToolIterations *int            `yaml:"max_tool_iterations,omitempty"`
	SystemPrompt string             `yaml:"system_prompt,omitempty"`
	Prompt       string             `yaml:"prompt,omitempty"` // alias of system_prompt
	PlanFirst    *bool              `yaml:"plan_first,omitempty"`
	Specialty    string             `yaml:"specialty,omitempty"`
	DelegateTo   []string           `yaml:"delegate_to,omitempty"`
	Skills       string             `yaml:"skills,omitempty"`
	Capabilities AgentCapabilities  `yaml:"capabilities,omitempty"`
	Modules      AgentModules       `yaml:"modules,omitempty"`
	Pool         *AgentPoolConfig   `yaml:"pool,omitempty"`
	Coordination *CoordinationBlock `yaml:"coordination,omitempty"`
	Instructions *InstructionsBlock `yaml:"instructions,omitempty"`
	// Context : per-agent context sections, layered ON TOP of the app-level
	// context (a section sharing an id overrides the app's).
	Context      *ContextBlock      `yaml:"context,omitempty"`
	Hooks        []Hook             `yaml:"hooks,omitempty"`
}

// AgentModules accepts three YAML shapes:
//   - [filesystem, shell]                       (list of strings)
//   - [{filesystem: [read, grep]}]              (list of single-key maps)
//   - {filesystem: [read, grep], shell: [exec]} (top-level map)
type AgentModules []ModuleRef

func (m *AgentModules) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	switch node.Kind {
	case yaml.SequenceNode:
		for _, item := range node.Content {
			var ref ModuleRef
			if err := ref.UnmarshalYAML(item); err != nil {
				return err
			}
			*m = append(*m, ref)
		}
		return nil
	case yaml.MappingNode:
		for i := 0; i < len(node.Content)-1; i += 2 {
			k, v := node.Content[i], node.Content[i+1]
			if k.Kind != yaml.ScalarNode {
				continue
			}
			ref := ModuleRef{ID: k.Value}
			if v.Kind == yaml.SequenceNode {
				for _, it := range v.Content {
					if it.Kind == yaml.ScalarNode {
						ref.Tools = append(ref.Tools, it.Value)
					}
				}
			}
			*m = append(*m, ref)
		}
		return nil
	default:
		return fmt.Errorf("agent modules: expected sequence or mapping, got kind %d", node.Kind)
	}
}

// ModuleRef accepts either "filesystem" or {filesystem: [read, grep]}.
type ModuleRef struct {
	ID    string
	Tools []string
}

func (m *ModuleRef) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	switch node.Kind {
	case yaml.ScalarNode:
		m.ID = node.Value
		return nil
	case yaml.MappingNode:
		if len(node.Content) < 2 {
			return fmt.Errorf("module ref: expected single-key mapping, got %d entries", len(node.Content)/2)
		}
		key, val := node.Content[0], node.Content[1]
		if key.Kind != yaml.ScalarNode {
			return fmt.Errorf("module ref: key must be a scalar string")
		}
		m.ID = key.Value
		if val.Kind == yaml.SequenceNode {
			for _, it := range val.Content {
				if it.Kind == yaml.ScalarNode {
					m.Tools = append(m.Tools, it.Value)
				}
			}
		}
		return nil
	default:
		return fmt.Errorf("module ref: expected scalar or mapping, got kind %d", node.Kind)
	}
}

type CoordinationBlock struct {
	DelegateTo []string         `yaml:"delegate_to,omitempty"`
	Pool       *AgentPoolConfig `yaml:"pool,omitempty"`
}

type InstructionsBlock struct {
	File         string            `yaml:"file,omitempty"`
	Capabilities AgentCapabilities `yaml:"capabilities,omitempty"`
	Specialty    string            `yaml:"specialty,omitempty"`
}

type AgentPoolConfig struct {
	MaxWorkers int  `yaml:"max_workers,omitempty"`
	Progress   bool `yaml:"progress,omitempty"`
	AutoRetry  int  `yaml:"auto_retry,omitempty"`
}

type Brain struct {
	ProviderID string `yaml:"provider_id,omitempty"`
	Provider   string `yaml:"provider,omitempty"`
	// Model is the DEFAULT model. Kind is the modality this brain operates on
	// (chat|image|audio|video|embedding) — it constrains which models a session
	// may switch to. Models lists the declared alternatives (same provider): in
	// direct/BYOK mode those are the ONLY switchable targets ; in gateway mode a
	// session may switch to ANY gateway model whose kind == Kind.
	Model            string         `yaml:"model,omitempty"`
	Kind             string         `yaml:"kind,omitempty"`
	Models           []string       `yaml:"models,omitempty"`
	Backend          Backend        `yaml:"backend,omitempty"`
	Config           map[string]any `yaml:"config,omitempty"`
	Credential       any            `yaml:"credential,omitempty"`
	Temperature      *float64       `yaml:"temperature,omitempty"`
	MaxTokens        *int           `yaml:"max_tokens,omitempty"`
	TopP             *float64       `yaml:"top_p,omitempty"`
	Timeout          *float64       `yaml:"timeout,omitempty"`
	NativeToolUse    *bool          `yaml:"native_tool_use,omitempty"`
	Context          *ContextConfig `yaml:"context,omitempty"`
	Fallback         *Brain         `yaml:"fallback,omitempty"`
	Vision           *bool          `yaml:"vision,omitempty"`
	ImageGeneration  bool           `yaml:"image_generation,omitempty"`
	ImageDetail      ImageDetail    `yaml:"image_detail,omitempty"`
	MaxImagesPerTurn int            `yaml:"max_images_per_turn,omitempty"`
}
