package schema

type RuntimeBlock struct {
	Mode                     Mode                 `yaml:"mode,omitempty" json:"mode,omitempty"`
	EntryAgent               string               `yaml:"entry_agent,omitempty" json:"entry_agent,omitempty"`
	MaxTurns                 int                  `yaml:"max_turns,omitempty" json:"max_turns,omitempty"`
	Timeout                  float64              `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	WorkdirMode              WorkdirMode          `yaml:"workdir_mode,omitempty" json:"workdir_mode,omitempty"`
	Workdir                  string               `yaml:"workdir,omitempty" json:"workdir,omitempty"`
	Modes                    map[string]ModeDef   `yaml:"modes,omitempty" json:"modes,omitempty"`
	Input                    *InputConfig         `yaml:"input,omitempty" json:"input,omitempty"`
	Output                   *OutputConfig        `yaml:"output,omitempty" json:"output,omitempty"`
	Triggers                 []TriggerConfig      `yaml:"triggers,omitempty" json:"triggers,omitempty"`
	SessionMode              SessionMode          `yaml:"session_mode,omitempty" json:"session_mode,omitempty"`
	MaxSessionsPerUser       int                  `yaml:"max_sessions_per_user,omitempty" json:"max_sessions_per_user,omitempty"`
	MaxConcurrentActivations int                  `yaml:"max_concurrent_activations,omitempty" json:"max_concurrent_activations,omitempty"`
	PayloadSchema            *PayloadSchemaConfig `yaml:"payload_schema,omitempty" json:"payload_schema,omitempty"`
	Pipeline                 []PipelineStep       `yaml:"pipeline,omitempty" json:"pipeline,omitempty"`
	Context                  *ContextConfig       `yaml:"context,omitempty" json:"context,omitempty"`
	ProjectMemory            string               `yaml:"project_memory,omitempty" json:"project_memory,omitempty"`
	DirectModules            []string             `yaml:"direct_modules,omitempty" json:"direct_modules,omitempty"`
	ToolInjection            ToolInjection        `yaml:"tool_injection,omitempty" json:"tool_injection,omitempty"`
	Hooks                    []Hook               `yaml:"hooks,omitempty" json:"hooks,omitempty"`
	// MaxStopRetries caps how many times a `stop` hook may hold a single turn
	// open (veto the model's attempt to finish + inject a steering directive)
	// before the runtime lets the turn end regardless. nil = the runtime
	// default (2) ; 0 disables stop-hook holds entirely.
	MaxStopRetries           *int                 `yaml:"max_stop_retries,omitempty" json:"max_stop_retries,omitempty"`
	Watchers                 bool                 `yaml:"watchers,omitempty" json:"watchers,omitempty"`
	Scheduler                bool                 `yaml:"scheduler,omitempty" json:"scheduler,omitempty"`
	DefaultChannel           string               `yaml:"default_channel,omitempty" json:"default_channel,omitempty"`
	Middleware               []MiddlewareEntry    `yaml:"middleware,omitempty" json:"middleware,omitempty"`
	Workbench                *bool                `yaml:"workbench,omitempty" json:"workbench,omitempty"`
	WorkbenchMaxChars        int                  `yaml:"workbench_max_chars,omitempty" json:"workbench_max_chars,omitempty"`
	WorkbenchReflection      *bool                `yaml:"workbench_reflection,omitempty" json:"workbench_reflection,omitempty"`
	WorkbenchErrorMemory     *bool                `yaml:"workbench_error_memory,omitempty" json:"workbench_error_memory,omitempty"`
	Flow                     *FlowConfig          `yaml:"flow,omitempty" json:"flow,omitempty"`
	ProjectMemoryPath        string               `yaml:"-" json:"-"`
	// MidTurnMessages decides what happens to a message sent WHILE a turn runs:
	//
	//   "queue"  (default) — it waits for the turn to finish, then runs as its
	//                        own turn. The running agent never sees it.
	//   "inject"           — it enters the RUNNING turn at the next safe
	//                        boundary (after the current tool's results are
	//                        persisted, so the provider contract holds) and no
	//                        follow-up turn is scheduled.
	//
	// Before this existed both happened at once: the running turn saw the
	// message AND a follow-up turn replayed it.
	MidTurnMessages MidTurnMode `yaml:"mid_turn_messages,omitempty" json:"mid_turn_messages,omitempty"`
	// DefaultMode names the mode a session starts on. Declared, not guessed:
	// before it existed the default was hard-coded to "auto when present, else
	// first declared", so an app could not open in `plan` without dropping or
	// reordering its modes. Ignored when it names a mode the app doesn't have.
	DefaultMode string `yaml:"default_mode,omitempty" json:"default_mode,omitempty"`
	// ModesOrder is the YAML insertion order of the Modes map keys, captured
	// at compile time (Go maps don't preserve order). The mode default-policy
	// ("first declared when no auto") needs it ; without it the default would
	// be non-deterministic.
	//
	// It MUST be serialized: the compiled app is stored as JSON (codegen/codec)
	// and `json:"-"` dropped the order on write. Reloading then rebuilt it by
	// ranging the Modes map — Go randomizes that — so both the picker order and
	// the default mode changed between daemon restarts. Still `yaml:"-"`: it is
	// derived from the document, never authored by hand.
	ModesOrder []string `yaml:"-" json:"modes_order,omitempty"`
}

type ModeDef struct {
	Label           string            `yaml:"label,omitempty" json:"label,omitempty"`
	Description     string            `yaml:"description,omitempty" json:"description,omitempty"`
	Icon            string            `yaml:"icon,omitempty" json:"icon,omitempty"`
	Accent          string            `yaml:"accent,omitempty" json:"accent,omitempty"`
	MaxTurns        *int              `yaml:"max_turns,omitempty" json:"max_turns,omitempty"`
	Timeout         *float64          `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	// NOTE: there used to be a `workspace_mode` here. It was copied into
	// EffectiveTurn and read by nobody — no allowed values, no validation, no
	// documentation, no app declaring it. Removed rather than left promising a
	// behaviour that was never designed. (App-level `workdir_mode` is a
	// different, real thing.) YAML decoding is lenient: an app still declaring
	// it is ignored, not rejected.
	SystemPrompt    string            `yaml:"system_prompt,omitempty" json:"system_prompt,omitempty"`
	ToolGrants      []CapabilityGrant `yaml:"tool_grants,omitempty" json:"tool_grants,omitempty"`
	BehaviorProfile string            `yaml:"behavior_profile,omitempty" json:"behavior_profile,omitempty"`
}

type InputConfig struct {
	Type        InputType `yaml:"type,omitempty" json:"type,omitempty"`
	Accept      []string  `yaml:"accept,omitempty" json:"accept,omitempty"`
	MaxSize     string    `yaml:"max_size,omitempty" json:"max_size,omitempty"`
	Description string    `yaml:"description,omitempty" json:"description,omitempty"`
	Required    *bool     `yaml:"required,omitempty" json:"required,omitempty"`
}

type OutputConfig struct {
	Type        OutputType     `yaml:"type,omitempty" json:"type,omitempty"`
	Format      string         `yaml:"format,omitempty" json:"format,omitempty"`
	Description string         `yaml:"description,omitempty" json:"description,omitempty"`
	SchemaDef   map[string]any `yaml:"schema_def,omitempty" json:"schema_def,omitempty"`
}

type TriggerConfig struct {
	ID         string      `yaml:"id" json:"id"`
	Type       TriggerType `yaml:"type" json:"type"`
	Schedule   string      `yaml:"schedule,omitempty" json:"schedule,omitempty"`
	Paths      []string    `yaml:"paths,omitempty" json:"paths,omitempty"`
	Path       string      `yaml:"path,omitempty" json:"path,omitempty"`
	Method     HTTPMethod  `yaml:"method,omitempty" json:"method,omitempty"`
	Port       int         `yaml:"port,omitempty" json:"port,omitempty"`
	Message    string      `yaml:"message,omitempty" json:"message,omitempty"`
	Routing    Routing     `yaml:"routing,omitempty" json:"routing,omitempty"`
	RoutingKey string      `yaml:"routing_key,omitempty" json:"routing_key,omitempty"`
}

type PipelineStep struct {
	App      string `yaml:"app" json:"app"`
	Input    string `yaml:"input,omitempty" json:"input,omitempty"`
	OutputAs string `yaml:"output_as,omitempty" json:"output_as,omitempty"`
	Optional bool   `yaml:"optional,omitempty" json:"optional,omitempty"`
}

type PayloadSchemaConfig struct {
	Required bool                    `yaml:"required,omitempty" json:"required,omitempty"`
	Prompt   map[string]any          `yaml:"prompt,omitempty" json:"prompt,omitempty"`
	Metadata []PayloadFieldConfig    `yaml:"metadata,omitempty" json:"metadata,omitempty"`
	Files    []PayloadFileRuleConfig `yaml:"files,omitempty" json:"files,omitempty"`
}

type PayloadFieldConfig struct {
	Name        string           `yaml:"name" json:"name"`
	Label       string           `yaml:"label,omitempty" json:"label,omitempty"`
	Type        PayloadFieldType `yaml:"type,omitempty" json:"type,omitempty"`
	Required    bool             `yaml:"required,omitempty" json:"required,omitempty"`
	Default     any              `yaml:"default,omitempty" json:"default,omitempty"`
	Description string           `yaml:"description,omitempty" json:"description,omitempty"`
	Placeholder string           `yaml:"placeholder,omitempty" json:"placeholder,omitempty"`
	Options     []string         `yaml:"options,omitempty" json:"options,omitempty"`
	Min         *float64         `yaml:"min,omitempty" json:"min,omitempty"`
	Max         *float64         `yaml:"max,omitempty" json:"max,omitempty"`
}

type PayloadFileRuleConfig struct {
	Name        string   `yaml:"name" json:"name"`
	Label       string   `yaml:"label,omitempty" json:"label,omitempty"`
	Description string   `yaml:"description,omitempty" json:"description,omitempty"`
	Required    bool     `yaml:"required,omitempty" json:"required,omitempty"`
	MIME        []string `yaml:"mime,omitempty" json:"mime,omitempty"`
	MaxSizeMB   float64  `yaml:"max_size_mb,omitempty" json:"max_size_mb,omitempty"`
	MaxCount    int      `yaml:"max_count,omitempty" json:"max_count,omitempty"`
}

type ContextConfig struct {
	MaxTokens          int             `yaml:"max_tokens,omitempty" json:"max_tokens,omitempty"`
	OutputReserved     int             `yaml:"output_reserved,omitempty" json:"output_reserved,omitempty"`
	Strategy           ContextStrategy `yaml:"strategy,omitempty" json:"strategy,omitempty"`
	KeepRecent         int             `yaml:"keep_recent,omitempty" json:"keep_recent,omitempty"`
	CompressionTrigger float64         `yaml:"compression_trigger,omitempty" json:"compression_trigger,omitempty"`
	SummaryMaxTokens   int             `yaml:"summary_max_tokens,omitempty" json:"summary_max_tokens,omitempty"`
	AutoCompact        *bool           `yaml:"auto_compact,omitempty" json:"auto_compact,omitempty"`
	SummaryBrain       *Brain          `yaml:"summary_brain,omitempty" json:"summary_brain,omitempty"`
}

type MiddlewareEntry struct {
	Name    string         `yaml:"name" json:"name"`
	Enabled *bool          `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Config  map[string]any `yaml:"config,omitempty" json:"config,omitempty"`
}
