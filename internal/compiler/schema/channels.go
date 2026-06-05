package schema

type ChannelInstanceConfig struct {
	Type         string              `yaml:"type"`
	Config       map[string]any      `yaml:"config,omitempty"`
	UserResolver *UserResolverConfig `yaml:"user_resolver,omitempty"`
}

type UserResolverConfig struct {
	Module   string            `yaml:"module"`
	Action   string            `yaml:"action"`
	Params   map[string]any    `yaml:"params,omitempty"`
	Mapping  map[string]string `yaml:"mapping,omitempty"`
	CacheTTL float64           `yaml:"cache_ttl,omitempty"`
}
