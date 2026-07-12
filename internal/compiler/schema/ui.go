package schema

type UIBlock struct {
	Theme          map[string]string   `yaml:"theme,omitempty" json:"theme,omitempty"`
	Features       map[string]bool     `yaml:"features,omitempty" json:"features,omitempty"`
	Workspace      *WorkspaceBlock     `yaml:"workspace,omitempty" json:"workspace,omitempty"`
	SlashCommands  []SlashCommand      `yaml:"slash_commands,omitempty" json:"slash_commands,omitempty"`
	QuickPrompts   []QuickPrompt       `yaml:"quick_prompts,omitempty" json:"quick_prompts,omitempty"`
	Greeting       string              `yaml:"greeting,omitempty" json:"greeting,omitempty"`
	Composer       *ChatComposerBlock  `yaml:"composer,omitempty" json:"composer,omitempty"`
	Layout         Layout              `yaml:"layout,omitempty" json:"layout,omitempty"`
	Density        Density             `yaml:"density,omitempty" json:"density,omitempty"`
	Thinking       *ChatThinkingBlock  `yaml:"thinking,omitempty" json:"thinking,omitempty"`
	ToolCalls      *ChatToolCallsBlock `yaml:"tool_calls,omitempty" json:"tool_calls,omitempty"`
	Visual         *ChatVisualBlock    `yaml:"visual,omitempty" json:"visual,omitempty"`
	Widgets        *WidgetsConfig      `yaml:"widgets,omitempty" json:"widgets,omitempty"`
	Slots          *SlotsConfig        `yaml:"slots,omitempty" json:"slots,omitempty"`
	Activity       *ActivityPanelBlock `yaml:"activity,omitempty" json:"activity,omitempty"`
	ToolRenderers  map[string]any      `yaml:"tool_renderers,omitempty" json:"tool_renderers,omitempty"`
	MessageActions map[string]any      `yaml:"message_actions,omitempty" json:"message_actions,omitempty"`
}

type SlashCommand struct {
	Command     string         `yaml:"command" json:"command"`
	Description string         `yaml:"description,omitempty" json:"description,omitempty"`
	Template    string         `yaml:"template,omitempty" json:"template,omitempty"`
	Action      map[string]any `yaml:"action,omitempty" json:"action,omitempty"` // {type: builtin, name: help} etc.
}

type WorkspaceBlock struct {
	RenderMode          RenderMode          `yaml:"render_mode,omitempty" json:"render_mode,omitempty"`
	EntryFile           string              `yaml:"entry_file,omitempty" json:"entry_file,omitempty"`
	Title               string              `yaml:"title,omitempty" json:"title,omitempty"`
	Position            Position            `yaml:"position,omitempty" json:"position,omitempty"`
	WidthPct            int                 `yaml:"width_pct,omitempty" json:"width_pct,omitempty"`
	AutoOpenOnFirstTool *bool               `yaml:"auto_open_on_first_tool,omitempty" json:"auto_open_on_first_tool,omitempty"`
	DefaultOpen         bool                `yaml:"default_open,omitempty" json:"default_open,omitempty"`
	PreviewChrome       *PreviewChromeBlock `yaml:"preview_chrome,omitempty" json:"preview_chrome,omitempty"`
	DefaultView         WorkspaceView       `yaml:"default_view,omitempty" json:"default_view,omitempty"`
	HiddenViews         []WorkspaceView     `yaml:"hidden_views,omitempty" json:"hidden_views,omitempty"`
	// SourceControl opts this app's workspace into VCS integration (connect,
	// open/clone a repo, commit, push/pull). Empty → hidden. Currently only
	// "github". A dedicated opt-in, NOT inferred from having a workspace, so a
	// code view can exist without exposing GitHub.
	SourceControl string `yaml:"source_control,omitempty" json:"source_control,omitempty"`
}

type PreviewChromeBlock struct {
	Enabled        *bool      `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Refresh        *bool      `yaml:"refresh,omitempty" json:"refresh,omitempty"`
	OpenInNewTab   *bool      `yaml:"open_in_new_tab,omitempty" json:"open_in_new_tab,omitempty"`
	ViewportToggle bool       `yaml:"viewport_toggle,omitempty" json:"viewport_toggle,omitempty"`
	URLBar         URLBarMode `yaml:"url_bar,omitempty" json:"url_bar,omitempty"`
}

type ChatThinkingBlock struct {
	Visible          *bool `yaml:"visible,omitempty" json:"visible,omitempty"`
	CollapsedDefault *bool `yaml:"collapsed_default,omitempty" json:"collapsed_default,omitempty"`
}

type ChatToolCallsBlock struct {
	CollapsedDefault *bool                `yaml:"collapsed_default,omitempty" json:"collapsed_default,omitempty"`
	ShowSilent       *bool                `yaml:"show_silent,omitempty" json:"show_silent,omitempty"`
	InjectIntent     *bool                `yaml:"inject_intent,omitempty" json:"inject_intent,omitempty"`
	HideDetails      *bool                `yaml:"hide_details,omitempty" json:"hide_details,omitempty"`
	StrictMode       *bool                `yaml:"strict_mode,omitempty" json:"strict_mode,omitempty"`
	IntentPhrases    *IntentPhrasesConfig `yaml:"intent_phrases,omitempty" json:"intent_phrases,omitempty"`
}

type IntentPhrasesConfig struct {
	Source IntentPhrasesSource        `yaml:"source,omitempty" json:"source,omitempty"`
	LLM    *IntentPhrasesLLMConfig    `yaml:"llm,omitempty" json:"llm,omitempty"`
	Static *IntentPhrasesStaticConfig `yaml:"static,omitempty" json:"static,omitempty"`
}

type IntentPhrasesLLMConfig struct {
	GatewayModel   string  `yaml:"gateway_model,omitempty" json:"gateway_model,omitempty"`
	MaxPhrases     int     `yaml:"max_phrases,omitempty" json:"max_phrases,omitempty"`
	MinPhrases     int     `yaml:"min_phrases,omitempty" json:"min_phrases,omitempty"`
	TimeoutSeconds float64 `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`
	Prompt         string  `yaml:"prompt,omitempty" json:"prompt,omitempty"`
}

type IntentPhrasesStaticConfig struct {
	Phases map[string][]string `yaml:"phases,omitempty" json:"phases,omitempty"`
}

type ChatComposerBlock struct {
	FileUpload          *bool `yaml:"file_upload,omitempty" json:"file_upload,omitempty"`
	Voice               *bool `yaml:"voice,omitempty" json:"voice,omitempty"`
	SlashCommands       *bool `yaml:"slash_commands,omitempty" json:"slash_commands,omitempty"`
	QuickPromptsVisible *bool `yaml:"quick_prompts_visible,omitempty" json:"quick_prompts_visible,omitempty"`
}

type ChatVisualBlock struct {
	Accent              string          `yaml:"accent,omitempty" json:"accent,omitempty"`
	BubbleStyle         BubbleStyle     `yaml:"bubble_style,omitempty" json:"bubble_style,omitempty"`
	UserBubbleAlignment BubbleAlignment `yaml:"user_bubble_alignment,omitempty" json:"user_bubble_alignment,omitempty"`
}

type ActivityPanelBlock struct {
	Enabled         *bool    `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Position        Position `yaml:"position,omitempty" json:"position,omitempty"`
	Title           string   `yaml:"title,omitempty" json:"title,omitempty"`
	ShowRunning     *bool    `yaml:"show_running,omitempty" json:"show_running,omitempty"`
	ShowRecent      *bool    `yaml:"show_recent,omitempty" json:"show_recent,omitempty"`
	ShowStats       *bool    `yaml:"show_stats,omitempty" json:"show_stats,omitempty"`
	ShowBgTasks     *bool    `yaml:"show_bg_tasks,omitempty" json:"show_bg_tasks,omitempty"`
	MaxRecent       int      `yaml:"max_recent,omitempty" json:"max_recent,omitempty"`
	AutoOpenOnSpawn bool     `yaml:"auto_open_on_spawn,omitempty" json:"auto_open_on_spawn,omitempty"`
}

type SlotsConfig struct {
	Header       *SlotEntry `yaml:"header,omitempty" json:"header,omitempty"`
	SidebarLeft  *SlotEntry `yaml:"sidebar_left,omitempty" json:"sidebar_left,omitempty"`
	SidebarRight *SlotEntry `yaml:"sidebar_right,omitempty" json:"sidebar_right,omitempty"`
	FooterLeft   *SlotEntry `yaml:"footer_left,omitempty" json:"footer_left,omitempty"`
	FooterRight  *SlotEntry `yaml:"footer_right,omitempty" json:"footer_right,omitempty"`
}

type SlotEntry struct {
	Kind string `yaml:"kind,omitempty" json:"kind,omitempty"`
	Ref  string `yaml:"ref,omitempty" json:"ref,omitempty"`
}

type WidgetsConfig struct {
	Version       int                       `yaml:"version,omitempty" json:"version,omitempty"`
	ChatSide      map[string]any            `yaml:"chat_side,omitempty" json:"chat_side,omitempty"`
	WorkspaceTabs []map[string]any          `yaml:"workspace_tabs,omitempty" json:"workspace_tabs,omitempty"`
	Modals        map[string]map[string]any `yaml:"modals,omitempty" json:"modals,omitempty"`
	Inline        map[string]map[string]any `yaml:"inline,omitempty" json:"inline,omitempty"`
}
