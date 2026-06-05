package schema

type SecurityBlock struct {
	Behavior          *BehaviorConfig          `yaml:"behavior,omitempty"`
	Sandbox           *SandboxConfig           `yaml:"sandbox,omitempty"`
	CredentialsSchema *CredentialsSchemaConfig `yaml:"credentials_schema,omitempty"`
}
