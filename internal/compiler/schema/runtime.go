package schema

type RuntimeBlock struct {
	Mode                     Mode                 `yaml:"mode,omitempty"`
	EntryAgent               string               `yaml:"entry_agent,omitempty"`
	MaxTurns                 int                  `yaml:"max_turns,omitempty"`
	Timeout                  float64              `yaml:"timeout,omitempty"`
	WorkdirMode              WorkdirMode          `yaml:"workdir_mode,omitempty"`
	Workdir                  string               `yaml:"workdir,omitempty"`
	Modes                    map[string]ModeDef   `yaml:"modes,omitempty"`
	Input                    *InputConfig         `yaml:"input,omitempty"`
	Output                   *OutputConfig        `yaml:"output,omitempty"`
	Triggers                 []TriggerConfig      `yaml:"triggers,omitempty"`
	SessionMode              SessionMode          `yaml:"session_mode,omitempty"`
	MaxSessionsPerUser       int                  `yaml:"max_sessions_per_user,omitempty"`
	MaxConcurrentActivations int                  `yaml:"max_concurrent_activations,omitempty"`
	PayloadSchema            *PayloadSchemaConfig `yaml:"payload_schema,omitempty"`
	Pipeline                 []PipelineStep       `yaml:"pipeline,omitempty"`
	Context                  *ContextConfig       `yaml:"context,omitempty"`
	ProjectMemory            string               `yaml:"project_memory,omitempty"`
	DirectModules            []string             `yaml:"direct_modules,omitempty"`
	ToolInjection            ToolInjection        `yaml:"tool_injection,omitempty"`
	Hooks                    []Hook               `yaml:"hooks,omitempty"`
	// MaxStopRetries caps how many times a `stop` hook may hold a single turn
	// open (veto the model's attempt to finish + inject a steering directive)
	// before the runtime lets the turn end regardless. nil = the runtime
	// default (2) ; 0 disables stop-hook holds entirely.
	MaxStopRetries           *int                 `yaml:"max_stop_retries,omitempty"`
	Watchers                 bool                 `yaml:"watchers,omitempty"`
	Scheduler                bool                 `yaml:"scheduler,omitempty"`
	DefaultChannel           string               `yaml:"default_channel,omitempty"`
	Middleware               []MiddlewareEntry    `yaml:"middleware,omitempty"`
	Workbench                *bool                `yaml:"workbench,omitempty"`
	WorkbenchMaxChars        int                  `yaml:"workbench_max_chars,omitempty"`
	WorkbenchReflection      *bool                `yaml:"workbench_reflection,omitempty"`
	WorkbenchErrorMemory     *bool                `yaml:"workbench_error_memory,omitempty"`
	Flow                     *FlowConfig          `yaml:"flow,omitempty"`
	ProjectMemoryPath        string               `yaml:"-"`
	// ModesOrder is the YAML insertion order of the Modes map keys, captured
	// at compile time (Go maps don't preserve order). The mode default-policy
	// ("first declared when no auto") needs it ; without it the default would
	// be non-deterministic.
	ModesOrder []string `yaml:"-"`
}

type ModeDef struct {
	Label           string            `yaml:"label,omitempty"`
	Description     string            `yaml:"description,omitempty"`
	Icon            string            `yaml:"icon,omitempty"`
	Accent          string            `yaml:"accent,omitempty"`
	MaxTurns        *int              `yaml:"max_turns,omitempty"`
	Timeout         *float64          `yaml:"timeout,omitempty"`
	WorkspaceMode   *string           `yaml:"workspace_mode,omitempty"`
	SystemPrompt    string            `yaml:"system_prompt,omitempty"`
	ToolGrants      []CapabilityGrant `yaml:"tool_grants,omitempty"`
	BehaviorProfile string            `yaml:"behavior_profile,omitempty"`
}

type InputConfig struct {
	Type        InputType `yaml:"type,omitempty"`
	Accept      []string  `yaml:"accept,omitempty"`
	MaxSize     string    `yaml:"max_size,omitempty"`
	Description string    `yaml:"description,omitempty"`
	Required    *bool     `yaml:"required,omitempty"`
}

type OutputConfig struct {
	Type        OutputType     `yaml:"type,omitempty"`
	Format      string         `yaml:"format,omitempty"`
	Description string         `yaml:"description,omitempty"`
	SchemaDef   map[string]any `yaml:"schema_def,omitempty"`
}

type TriggerConfig struct {
	ID         string      `yaml:"id"`
	Type       TriggerType `yaml:"type"`
	Schedule   string      `yaml:"schedule,omitempty"`
	Paths      []string    `yaml:"paths,omitempty"`
	Path       string      `yaml:"path,omitempty"`
	Method     HTTPMethod  `yaml:"method,omitempty"`
	Port       int         `yaml:"port,omitempty"`
	Message    string      `yaml:"message,omitempty"`
	Routing    Routing     `yaml:"routing,omitempty"`
	RoutingKey string      `yaml:"routing_key,omitempty"`
}

type PipelineStep struct {
	App      string `yaml:"app"`
	Input    string `yaml:"input,omitempty"`
	OutputAs string `yaml:"output_as,omitempty"`
	Optional bool   `yaml:"optional,omitempty"`
}

type PayloadSchemaConfig struct {
	Required bool                    `yaml:"required,omitempty"`
	Prompt   map[string]any          `yaml:"prompt,omitempty"`
	Metadata []PayloadFieldConfig    `yaml:"metadata,omitempty"`
	Files    []PayloadFileRuleConfig `yaml:"files,omitempty"`
}

type PayloadFieldConfig struct {
	Name        string           `yaml:"name"`
	Label       string           `yaml:"label,omitempty"`
	Type        PayloadFieldType `yaml:"type,omitempty"`
	Required    bool             `yaml:"required,omitempty"`
	Default     any              `yaml:"default,omitempty"`
	Description string           `yaml:"description,omitempty"`
	Placeholder string           `yaml:"placeholder,omitempty"`
	Options     []string         `yaml:"options,omitempty"`
	Min         *float64         `yaml:"min,omitempty"`
	Max         *float64         `yaml:"max,omitempty"`
}

type PayloadFileRuleConfig struct {
	Name        string   `yaml:"name"`
	Label       string   `yaml:"label,omitempty"`
	Description string   `yaml:"description,omitempty"`
	Required    bool     `yaml:"required,omitempty"`
	MIME        []string `yaml:"mime,omitempty"`
	MaxSizeMB   float64  `yaml:"max_size_mb,omitempty"`
	MaxCount    int      `yaml:"max_count,omitempty"`
}

type ContextConfig struct {
	MaxTokens          int             `yaml:"max_tokens,omitempty"`
	OutputReserved     int             `yaml:"output_reserved,omitempty"`
	Strategy           ContextStrategy `yaml:"strategy,omitempty"`
	KeepRecent         int             `yaml:"keep_recent,omitempty"`
	CompressionTrigger float64         `yaml:"compression_trigger,omitempty"`
	SummaryMaxTokens   int             `yaml:"summary_max_tokens,omitempty"`
	AutoCompact        *bool           `yaml:"auto_compact,omitempty"`
	SummaryBrain       *Brain          `yaml:"summary_brain,omitempty"`
}

type MiddlewareEntry struct {
	Name    string         `yaml:"name"`
	Enabled *bool          `yaml:"enabled,omitempty"`
	Config  map[string]any `yaml:"config,omitempty"`
}
