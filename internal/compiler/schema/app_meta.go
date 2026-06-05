package schema

type AppMeta struct {
	AppID           string            `yaml:"app_id"`
	Name            string            `yaml:"name"`
	ShortName       string            `yaml:"short_name,omitempty"`
	Version         string            `yaml:"version,omitempty"`
	SchemaVersion   string            `yaml:"schema_version,omitempty"`
	Description     string            `yaml:"description,omitempty"`
	Author          string            `yaml:"author,omitempty"`
	Tags            []string          `yaml:"tags,omitempty"`
	Icon            string            `yaml:"icon,omitempty"`
	Color           string            `yaml:"color,omitempty"`
	Category        Category          `yaml:"category,omitempty"`
	QuickPrompts    []QuickPrompt     `yaml:"quick_prompts,omitempty"`
	Attachments     Attachments       `yaml:"attachments,omitempty"`
	AttachmentsMode AttachmentsMode   `yaml:"attachments_mode,omitempty"`
	Features        map[string]bool   `yaml:"features,omitempty"`
	Theme           map[string]string `yaml:"theme,omitempty"`
	Mode            string            `yaml:"mode,omitempty"` // some manifests put runtime.mode at app level
}

type QuickPrompt struct {
	Label   string `yaml:"label"`
	Message string `yaml:"message"`
	Icon    string `yaml:"icon,omitempty"`
}
