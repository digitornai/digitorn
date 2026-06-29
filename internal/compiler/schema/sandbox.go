package schema

type SandboxConfig struct {
	Level             SandboxLevel   `yaml:"level,omitempty" json:"level,omitempty"`
	PoolSize          int            `yaml:"pool_size,omitempty" json:"pool_size,omitempty"`
	PoolMax           int            `yaml:"pool_max,omitempty" json:"pool_max,omitempty"`
	Namespaces        []string       `yaml:"namespaces,omitempty" json:"namespaces,omitempty"`
	WorkspaceSnapshot bool           `yaml:"workspace_snapshot,omitempty" json:"workspace_snapshot,omitempty"`
	Audit             bool           `yaml:"audit,omitempty" json:"audit,omitempty"`
	SessionTimeout    int            `yaml:"session_timeout,omitempty" json:"session_timeout,omitempty"`
	IdleTimeout       int            `yaml:"idle_timeout,omitempty" json:"idle_timeout,omitempty"`
	AllowPaths        []string       `yaml:"allow_paths,omitempty" json:"allow_paths,omitempty"`
	Resources         map[string]any `yaml:"resources,omitempty" json:"resources,omitempty"`
}
