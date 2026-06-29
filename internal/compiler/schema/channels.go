package schema

type ChannelInstanceConfig struct {
	Type         string              `yaml:"type" json:"type"`
	Config       map[string]any      `yaml:"config,omitempty" json:"config,omitempty"`
	UserResolver *UserResolverConfig `yaml:"user_resolver,omitempty" json:"user_resolver,omitempty"`
}

type UserResolverConfig struct {
	Module   string            `yaml:"module" json:"module"`
	Action   string            `yaml:"action" json:"action"`
	Params   map[string]any    `yaml:"params,omitempty" json:"params,omitempty"`
	Mapping  map[string]string `yaml:"mapping,omitempty" json:"mapping,omitempty"`
	CacheTTL float64           `yaml:"cache_ttl,omitempty" json:"cache_ttl,omitempty"`
}
