package schema

type DevBlock struct {
	Skills          []SkillEntry      `yaml:"skills,omitempty"`
	AllowUserSkills bool              `yaml:"allow_user_skills,omitempty"`
	Variables       map[string]string `yaml:"variables,omitempty"`
	Include         *IncludeBlock     `yaml:"include,omitempty"`
}

type SkillEntry struct {
	Command     string `yaml:"command"`
	Description string `yaml:"description,omitempty"`
	Path        string `yaml:"path"`
}

type IncludeBlock struct {
	Agents any `yaml:"agents,omitempty"`
	Hooks  any `yaml:"hooks,omitempty"`
}
