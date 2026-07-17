package agent

type Role string

const (
	RoleWorker      Role = "worker"
	RoleCoordinator Role = "coordinator"
	RoleSpecialist  Role = "specialist"
)

type Brain struct {
	Provider    string         `yaml:"provider" json:"provider"`
	Model       string         `yaml:"model" json:"model"`
	Backend     string         `yaml:"backend,omitempty" json:"backend,omitempty"`
	Temperature float64        `yaml:"temperature,omitempty" json:"temperature,omitempty"`
	MaxTokens   int            `yaml:"max_tokens,omitempty" json:"max_tokens,omitempty"`
	Config      map[string]any `yaml:"config,omitempty" json:"config,omitempty"`
}

type Tools struct {
	Modules      []string `yaml:"modules" json:"modules"`
	Capabilities []string `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
}

type Definition struct {
	ID           string   `yaml:"id" json:"id"`
	Role         Role     `yaml:"role,omitempty" json:"role,omitempty"`
	Brain        Brain    `yaml:"brain" json:"brain"`
	SystemPrompt string   `yaml:"system_prompt,omitempty" json:"system_prompt,omitempty"`
	Instructions string   `yaml:"instructions,omitempty" json:"instructions,omitempty"`
	Tools        Tools    `yaml:"tools" json:"tools"`
	DelegateTo   []string `yaml:"delegate_to,omitempty" json:"delegate_to,omitempty"`
}
