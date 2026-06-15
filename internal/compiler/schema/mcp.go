package schema

import "gopkg.in/yaml.v3"

type McpModuleConfig struct {
	Workspace   string           `yaml:"workspace,omitempty"`
	Servers     any              `yaml:"servers,omitempty"`
	Cache       MCPCacheConfig   `yaml:"cache,omitempty"`
	Middleware  []map[string]any `yaml:"middleware,omitempty"`
	AutoInstall bool             `yaml:"auto_install,omitempty"`
}

type MCPServerConfig struct {
	Transport      MCPTransport      `yaml:"transport,omitempty"`
	Command        string            `yaml:"command,omitempty"`
	Args           []string          `yaml:"args,omitempty"`
	Env            map[string]string `yaml:"env,omitempty"`
	URL            string            `yaml:"url,omitempty"`
	Headers        map[string]string `yaml:"headers,omitempty"`
	Timeout        float64           `yaml:"timeout,omitempty"`
	BufferSize     int               `yaml:"buffer_size,omitempty"`
	Auth           *MCPAuthConfig    `yaml:"auth,omitempty"`
	Examples       map[string]any    `yaml:"examples,omitempty"`
	RateLimitRPM   int               `yaml:"rate_limit_rpm,omitempty"`
	Via            string            `yaml:"via,omitempty"`
	SmitheryKey    string            `yaml:"smithery_key,omitempty"`
	SmitheryNS     string            `yaml:"smithery_namespace,omitempty"`
	SmitherySlug   string            `yaml:"smithery_slug,omitempty"`
	Sandbox        *MCPServerSandbox `yaml:"sandbox,omitempty"`
	Middleware     []map[string]any  `yaml:"middleware,omitempty"`
	CacheTTL       float64           `yaml:"cache_ttl,omitempty"`
	CacheableTools []string          `yaml:"cacheable_tools,omitempty"`
	Extra          map[string]any    `yaml:",inline"`
}

type MCPServerSandbox struct {
	Permissions  []string        `yaml:"permissions,omitempty"`
	Paths        MCPSandboxPaths `yaml:"paths,omitempty"`
	AllowedHosts []string        `yaml:"allowed_hosts,omitempty"`
	Extra        map[string]any  `yaml:",inline"`
}

type MCPSandboxPaths struct {
	Read  []string `yaml:"read,omitempty"`
	Write []string `yaml:"write,omitempty"`
}

type MCPCacheConfig struct {
	TTL     int   `yaml:"ttl,omitempty"`
	MaxSize int   `yaml:"max_size,omitempty"`
	Enabled *bool `yaml:"enabled,omitempty"`
}

type MCPAuthConfig struct {
	Type            string         `yaml:"type,omitempty"`
	Provider        string         `yaml:"provider,omitempty"`
	ClientID        string         `yaml:"client_id,omitempty"`
	ClientSecret    string         `yaml:"client_secret,omitempty"`
	Scopes          []string       `yaml:"scopes,omitempty"`
	RedirectURI     string         `yaml:"redirect_uri,omitempty"`
	AuthorizeURL    string         `yaml:"authorize_url,omitempty"`
	TokenURL        string         `yaml:"token_url,omitempty"`
	RevokeURL       string         `yaml:"revoke_url,omitempty"`
	PKCE            *bool          `yaml:"pkce,omitempty"`
	TokenAuthMethod string         `yaml:"token_auth_method,omitempty"`
	ExtraParams     map[string]any `yaml:"extra_params,omitempty"`
	EnvTokenVar     string         `yaml:"env_token_var,omitempty"`
	// Resource is the RFC 8707 resource indicator — the protected MCP server's
	// canonical URI. The MCP auth spec requires it on the authorize + token
	// requests so the issued token is bound to this resource. Auto-filled from
	// discovery; rarely set by hand.
	Resource string `yaml:"resource,omitempty"`
}

func defaultBareSandbox() *MCPServerSandbox {
	return &MCPServerSandbox{Permissions: []string{"process.exec", "net.http"}}
}

// NormalizeServers folds the three `servers` YAML shapes into one map. Bare/empty
// refs get the default sandbox; inline entries decode verbatim. Returns unparsable ids.
func NormalizeServers(raw any) (map[string]MCPServerConfig, []string) {
	out := map[string]MCPServerConfig{}
	bad := []string{}
	add := func(id string, v any) {
		if emptyServerValue(v) {
			out[id] = MCPServerConfig{Sandbox: defaultBareSandbox()}
			return
		}
		m, ok := v.(map[string]any)
		if !ok {
			bad = append(bad, id)
			return
		}
		c, err := decodeServer(m)
		if err != nil {
			bad = append(bad, id)
			return
		}
		out[id] = c
	}
	switch s := raw.(type) {
	case []any:
		for _, it := range s {
			switch e := it.(type) {
			case string:
				if e != "" {
					add(e, nil)
				}
			case map[string]any:
				for k, v := range e {
					add(k, v)
				}
			default:
				bad = append(bad, "")
			}
		}
	case map[string]any:
		for k, v := range s {
			add(k, v)
		}
	}
	return out, bad
}

func emptyServerValue(v any) bool {
	if v == nil {
		return true
	}
	if m, ok := v.(map[string]any); ok {
		return len(m) == 0
	}
	return false
}

func decodeServer(m map[string]any) (MCPServerConfig, error) {
	b, err := yaml.Marshal(m)
	if err != nil {
		return MCPServerConfig{}, err
	}
	var c MCPServerConfig
	if err := yaml.Unmarshal(b, &c); err != nil {
		return MCPServerConfig{}, err
	}
	return c, nil
}
