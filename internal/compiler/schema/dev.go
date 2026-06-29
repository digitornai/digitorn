package schema

type DevBlock struct {
	Skills          []SkillEntry      `yaml:"skills,omitempty" json:"skills,omitempty"`
	AllowUserSkills bool              `yaml:"allow_user_skills,omitempty" json:"allow_user_skills,omitempty"`
	Variables       map[string]string `yaml:"variables,omitempty" json:"variables,omitempty"`
	Include         *IncludeBlock     `yaml:"include,omitempty" json:"include,omitempty"`
}

type SkillEntry struct {
	Command     string `yaml:"command" json:"command"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Path        string `yaml:"path" json:"path"`
}

type IncludeBlock struct {
	Agents any `yaml:"agents,omitempty" json:"agents,omitempty"`
	Hooks  any `yaml:"hooks,omitempty" json:"hooks,omitempty"`
}
