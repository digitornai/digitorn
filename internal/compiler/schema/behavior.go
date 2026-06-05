package schema

type BehaviorConfig struct {
	Profile         string                   `yaml:"profile,omitempty"`
	Rules           map[string]any           `yaml:"rules,omitempty"`
	Custom          []BehaviorCustomRule     `yaml:"custom,omitempty"`
	RuleDefinitions []BehaviorRuleDefinition `yaml:"rule_definitions,omitempty"`
	StateTracking   *StateTrackingConfig     `yaml:"state_tracking,omitempty"`
	ClassifyTurns   bool                     `yaml:"classify_turns,omitempty"`
	Classifier      *ClassifierConfig        `yaml:"classifier,omitempty"`
	Brain           *Brain                   `yaml:"brain,omitempty"`
	UseAgentBrain   *bool                    `yaml:"use_agent_brain,omitempty"`
}

type BehaviorCustomRule map[string]any

type BehaviorRuleDefinition struct {
	ID          string         `yaml:"id"`
	Description string         `yaml:"description,omitempty"`
	Trigger     any            `yaml:"trigger,omitempty"`
	When        RuleWhen       `yaml:"when,omitempty"`
	Action      RuleAction     `yaml:"action,omitempty"`
	Condition   map[string]any `yaml:"condition,omitempty"`
	Message     string         `yaml:"message,omitempty"`
}

type StateTrackingConfig struct {
	Sets     map[string]StateTrackingSetConfig     `yaml:"sets,omitempty"`
	Counters map[string]StateTrackingCounterConfig `yaml:"counters,omitempty"`
	Flags    map[string]StateTrackingFlagConfig    `yaml:"flags,omitempty"`
}

type StateTrackingSetConfig struct {
	AddOn   []string `yaml:"add_on"`
	Target  string   `yaml:"target,omitempty"`
	Aliases []string `yaml:"aliases,omitempty"`
}

type StateTrackingCounterConfig struct {
	IncrementOn []string          `yaml:"increment_on"`
	ResetOn     []string          `yaml:"reset_on,omitempty"`
	ResetWhen   map[string]string `yaml:"reset_when,omitempty"`
}

type StateTrackingFlagConfig struct {
	SetOn   []string `yaml:"set_on"`
	UnsetOn []string `yaml:"unset_on,omitempty"`
}

type ClassifierConfig struct {
	Frequency         ClassifierFrequency `yaml:"frequency,omitempty"`
	FrequencyN        int                 `yaml:"frequency_n,omitempty"`
	SkipFollowups     *bool               `yaml:"skip_followups,omitempty"`
	Timeout           int                 `yaml:"timeout,omitempty"`
	ComplexityLevels  []any               `yaml:"complexity_levels,omitempty"`
	Approaches        []any               `yaml:"approaches,omitempty"`
	RiskLevels        []any               `yaml:"risk_levels,omitempty"`
	MaxDirectives     int                 `yaml:"max_directives,omitempty"`
	Context           *ClassifierContext  `yaml:"context,omitempty"`
	SystemPrompt      string              `yaml:"system_prompt,omitempty"`
	DirectivePrefix   string              `yaml:"directive_prefix,omitempty"`
	HighRiskWarning   string              `yaml:"high_risk_warning,omitempty"`
	HighRiskThreshold string              `yaml:"high_risk_threshold,omitempty"`
	DirectiveFooter   string              `yaml:"directive_footer,omitempty"`
}

type ClassifierContext struct {
	ToolInventory bool `yaml:"tool_inventory,omitempty"`
	SessionState  bool `yaml:"session_state,omitempty"`
	WorkspaceInfo bool `yaml:"workspace_info,omitempty"`
	RecentHistory bool `yaml:"recent_history,omitempty"`
	HistoryDepth  int  `yaml:"history_depth,omitempty"`
}
