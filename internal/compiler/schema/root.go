package schema

type AppDefinition struct {
	SchemaVersion int             `yaml:"schema_version,omitempty" json:"schema_version,omitempty"`
	App           AppMeta         `yaml:"app" json:"app"`
	Runtime       *RuntimeBlock   `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	Agents        []Agent         `yaml:"agents,omitempty" json:"agents,omitempty"`
	Tools         *ToolsBlock     `yaml:"tools,omitempty" json:"tools,omitempty"`
	Security      *SecurityBlock  `yaml:"security,omitempty" json:"security,omitempty"`
	UI            *UIBlock        `yaml:"ui,omitempty" json:"ui,omitempty"`
	Dev           *DevBlock       `yaml:"dev,omitempty" json:"dev,omitempty"`
	Flow          *FlowConfig     `yaml:"flow,omitempty" json:"flow,omitempty"`
	Templates     []TemplateBlock `yaml:"templates,omitempty" json:"templates,omitempty"`
	// Requirements : system binaries the app needs at runtime but does not ship.
	// The daemon provisions them out-of-band (consent-gated, async) and puts them
	// on the agent's PATH. See requirements.go.
	Requirements []Requirement `yaml:"requirements,omitempty" json:"requirements,omitempty"`
	// Context : app-wide system-prompt context sections injected each turn
	// (user/session data, date, custom blocks). Applies to every agent.
	Context *ContextBlock `yaml:"context,omitempty" json:"context,omitempty"`
	// Legacy top-level aliases for tools.modules and tools.capabilities; the
	// compiler folds them into Tools before downstream phases see the tree.
	ModulesTop      map[string]ModuleBlock `yaml:"modules,omitempty" json:"modules,omitempty"`
	CapabilitiesTop *CapabilitiesConfig    `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
}

type TemplateBlock struct {
	ID           string `yaml:"id" json:"id"`
	Name         string `yaml:"name" json:"name"`
	Description  string `yaml:"description,omitempty" json:"description,omitempty"`
	PreviewPath  string `yaml:"preview_path" json:"preview_path"`
	SeedDir      string `yaml:"seed_dir" json:"seed_dir"`
	SystemPrompt string `yaml:"system_prompt" json:"system_prompt"`
}
