package schema

type AppDefinition struct {
	SchemaVersion int             `yaml:"schema_version,omitempty"`
	App           AppMeta         `yaml:"app"`
	Runtime       *RuntimeBlock   `yaml:"runtime,omitempty"`
	Agents        []Agent         `yaml:"agents,omitempty"`
	Tools         *ToolsBlock     `yaml:"tools,omitempty"`
	Security      *SecurityBlock  `yaml:"security,omitempty"`
	UI            *UIBlock        `yaml:"ui,omitempty"`
	Dev           *DevBlock       `yaml:"dev,omitempty"`
	Flow          *FlowConfig     `yaml:"flow,omitempty"`
	Templates     []TemplateBlock `yaml:"templates,omitempty"`
	// Context : app-wide system-prompt context sections injected each turn
	// (user/session data, date, custom blocks). Applies to every agent.
	Context *ContextBlock `yaml:"context,omitempty"`
	// Legacy top-level aliases for tools.modules and tools.capabilities; the
	// compiler folds them into Tools before downstream phases see the tree.
	ModulesTop      map[string]ModuleBlock `yaml:"modules,omitempty"`
	CapabilitiesTop *CapabilitiesConfig    `yaml:"capabilities,omitempty"`
}

type TemplateBlock struct {
	ID           string `yaml:"id"`
	Name         string `yaml:"name"`
	Description  string `yaml:"description,omitempty"`
	PreviewPath  string `yaml:"preview_path"`
	SeedDir      string `yaml:"seed_dir"`
	SystemPrompt string `yaml:"system_prompt"`
}
