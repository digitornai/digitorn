package schema

type SandboxConfig struct {
	Level             SandboxLevel   `yaml:"level,omitempty"`
	PoolSize          int            `yaml:"pool_size,omitempty"`
	PoolMax           int            `yaml:"pool_max,omitempty"`
	Namespaces        []string       `yaml:"namespaces,omitempty"`
	WorkspaceSnapshot bool           `yaml:"workspace_snapshot,omitempty"`
	Audit             bool           `yaml:"audit,omitempty"`
	SessionTimeout    int            `yaml:"session_timeout,omitempty"`
	IdleTimeout       int            `yaml:"idle_timeout,omitempty"`
	AllowPaths        []string       `yaml:"allow_paths,omitempty"`
	Resources         map[string]any `yaml:"resources,omitempty"`
}
