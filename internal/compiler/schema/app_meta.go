package schema

type AppMeta struct {
	AppID           string            `yaml:"app_id" json:"app_id"`
	Name            string            `yaml:"name" json:"name"`
	ShortName       string            `yaml:"short_name,omitempty" json:"short_name,omitempty"`
	Version         string            `yaml:"version,omitempty" json:"version,omitempty"`
	SchemaVersion   string            `yaml:"schema_version,omitempty" json:"schema_version,omitempty"`
	Description     string            `yaml:"description,omitempty" json:"description,omitempty"`
	Author          string            `yaml:"author,omitempty" json:"author,omitempty"`
	Tags            []string          `yaml:"tags,omitempty" json:"tags,omitempty"`
	Icon            string            `yaml:"icon,omitempty" json:"icon,omitempty"`
	Color           string            `yaml:"color,omitempty" json:"color,omitempty"`
	Category        Category          `yaml:"category,omitempty" json:"category,omitempty"`
	QuickPrompts    []QuickPrompt     `yaml:"quick_prompts,omitempty" json:"quick_prompts,omitempty"`
	Attachments     Attachments       `yaml:"attachments,omitempty" json:"attachments,omitempty"`
	AttachmentsMode AttachmentsMode   `yaml:"attachments_mode,omitempty" json:"attachments_mode,omitempty"`
	Features        map[string]bool   `yaml:"features,omitempty" json:"features,omitempty"`
	Theme           map[string]string `yaml:"theme,omitempty" json:"theme,omitempty"`
	Mode            string            `yaml:"mode,omitempty" json:"mode,omitempty"` // some manifests put runtime.mode at app level
}

type QuickPrompt struct {
	Label   string `yaml:"label" json:"label"`
	Message string `yaml:"message" json:"message"`
	Icon    string `yaml:"icon,omitempty" json:"icon,omitempty"`
}
