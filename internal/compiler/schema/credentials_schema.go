package schema

type CredentialsSchemaConfig struct {
	Required  *bool                      `yaml:"required,omitempty"`
	Providers []CredentialProviderConfig `yaml:"providers,omitempty"`
}

type CredentialProviderConfig struct {
	Name          string                  `yaml:"name"`
	Label         string                  `yaml:"label,omitempty"`
	Type          CredentialType          `yaml:"type,omitempty"`
	Scope         CredentialScope         `yaml:"scope,omitempty"`
	Required      *bool                   `yaml:"required,omitempty"`
	Icon          string                  `yaml:"icon,omitempty"`
	DocsURL       string                  `yaml:"docs_url,omitempty"`
	Fields        []CredentialFieldConfig `yaml:"fields,omitempty"`
	OAuthProvider string                  `yaml:"oauth_provider,omitempty"`
	OAuthScopes   []string                `yaml:"oauth_scopes,omitempty"`
	Transport     MCPTransport            `yaml:"transport,omitempty"`
	Command       []string                `yaml:"command,omitempty"`
	URL           string                  `yaml:"url,omitempty"`
	EnvTemplate   map[string]string       `yaml:"env_template,omitempty"`
	HealthCheck   map[string]any          `yaml:"health_check,omitempty"`
	Test          map[string]any          `yaml:"test,omitempty"`
}

type CredentialFieldConfig struct {
	Name            string              `yaml:"name"`
	Label           string              `yaml:"label,omitempty"`
	Type            CredentialFieldType `yaml:"type,omitempty"`
	Required        bool                `yaml:"required,omitempty"`
	Default         any                 `yaml:"default,omitempty"`
	Description     string              `yaml:"description,omitempty"`
	Placeholder     string              `yaml:"placeholder,omitempty"`
	ValidationRegex string              `yaml:"validation_regex,omitempty"`
	Options         []string            `yaml:"options,omitempty"`
	Help            string              `yaml:"help,omitempty"`
}
