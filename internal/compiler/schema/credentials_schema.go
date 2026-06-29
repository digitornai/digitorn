package schema

type CredentialsSchemaConfig struct {
	Required  *bool                      `yaml:"required,omitempty" json:"required,omitempty"`
	Providers []CredentialProviderConfig `yaml:"providers,omitempty" json:"providers,omitempty"`
}

type CredentialProviderConfig struct {
	Name          string                  `yaml:"name" json:"name"`
	Label         string                  `yaml:"label,omitempty" json:"label,omitempty"`
	Type          CredentialType          `yaml:"type,omitempty" json:"type,omitempty"`
	Scope         CredentialScope         `yaml:"scope,omitempty" json:"scope,omitempty"`
	Required      *bool                   `yaml:"required,omitempty" json:"required,omitempty"`
	Icon          string                  `yaml:"icon,omitempty" json:"icon,omitempty"`
	DocsURL       string                  `yaml:"docs_url,omitempty" json:"docs_url,omitempty"`
	Fields        []CredentialFieldConfig `yaml:"fields,omitempty" json:"fields,omitempty"`
	OAuthProvider string                  `yaml:"oauth_provider,omitempty" json:"oauth_provider,omitempty"`
	OAuthScopes   []string                `yaml:"oauth_scopes,omitempty" json:"oauth_scopes,omitempty"`
	Transport     MCPTransport            `yaml:"transport,omitempty" json:"transport,omitempty"`
	Command       []string                `yaml:"command,omitempty" json:"command,omitempty"`
	URL           string                  `yaml:"url,omitempty" json:"url,omitempty"`
	EnvTemplate   map[string]string       `yaml:"env_template,omitempty" json:"env_template,omitempty"`
	HealthCheck   map[string]any          `yaml:"health_check,omitempty" json:"health_check,omitempty"`
	Test          map[string]any          `yaml:"test,omitempty" json:"test,omitempty"`
}

type CredentialFieldConfig struct {
	Name            string              `yaml:"name" json:"name"`
	Label           string              `yaml:"label,omitempty" json:"label,omitempty"`
	Type            CredentialFieldType `yaml:"type,omitempty" json:"type,omitempty"`
	Required        bool                `yaml:"required,omitempty" json:"required,omitempty"`
	Default         any                 `yaml:"default,omitempty" json:"default,omitempty"`
	Description     string              `yaml:"description,omitempty" json:"description,omitempty"`
	Placeholder     string              `yaml:"placeholder,omitempty" json:"placeholder,omitempty"`
	ValidationRegex string              `yaml:"validation_regex,omitempty" json:"validation_regex,omitempty"`
	Options         []string            `yaml:"options,omitempty" json:"options,omitempty"`
	Help            string              `yaml:"help,omitempty" json:"help,omitempty"`
}
