package schema

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

type SetupStep struct {
	Action string         `yaml:"action"`
	Params map[string]any `yaml:"params,omitempty"`
}
