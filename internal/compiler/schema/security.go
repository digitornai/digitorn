package schema

type SecurityBlock struct {
	Behavior          *BehaviorConfig          `yaml:"behavior,omitempty" json:"behavior,omitempty"`
	Sandbox           *SandboxConfig           `yaml:"sandbox,omitempty" json:"sandbox,omitempty"`
	CredentialsSchema *CredentialsSchemaConfig `yaml:"credentials_schema,omitempty" json:"credentials_schema,omitempty"`
}
