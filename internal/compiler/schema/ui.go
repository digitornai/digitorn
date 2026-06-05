package schema

type UIBlock struct {
	Theme          map[string]string   `yaml:"theme,omitempty"`
	Features       map[string]bool     `yaml:"features,omitempty"`
	Workspace      *WorkspaceBlock     `yaml:"workspace,omitempty"`
	SlashCommands  []SlashCommand      `yaml:"slash_commands,omitempty"`
	QuickPrompts   []QuickPrompt       `yaml:"quick_prompts,omitempty"`
	Greeting       string              `yaml:"greeting,omitempty"`
	Composer       *ChatComposerBlock  `yaml:"composer,omitempty"`
	Layout         Layout              `yaml:"layout,omitempty"`
	Density        Density             `yaml:"density,omitempty"`
	Thinking       *ChatThinkingBlock  `yaml:"thinking,omitempty"`
	ToolCalls      *ChatToolCallsBlock `yaml:"tool_calls,omitempty"`
	Visual         *ChatVisualBlock    `yaml:"visual,omitempty"`
	Widgets        *WidgetsConfig      `yaml:"widgets,omitempty"`
	Slots          *SlotsConfig        `yaml:"slots,omitempty"`
	Activity       *ActivityPanelBlock `yaml:"activity,omitempty"`
	ToolRenderers  map[string]any      `yaml:"tool_renderers,omitempty"`
	MessageActions map[string]any      `yaml:"message_actions,omitempty"`
}

type SlashCommand struct {
	Command     string         `yaml:"command"`
	Description string         `yaml:"description,omitempty"`
	Template    string         `yaml:"template,omitempty"`
	Action      map[string]any `yaml:"action,omitempty"` // {type: builtin, name: help} etc.
}

type WorkspaceBlock struct {
	RenderMode          RenderMode          `yaml:"render_mode,omitempty"`
	EntryFile           string              `yaml:"entry_file,omitempty"`
	Title               string              `yaml:"title,omitempty"`
	Position            Position            `yaml:"position,omitempty"`
	WidthPct            int                 `yaml:"width_pct,omitempty"`
	AutoOpenOnFirstTool *bool               `yaml:"auto_open_on_first_tool,omitempty"`
	DefaultOpen         bool                `yaml:"default_open,omitempty"`
	PreviewChrome       *PreviewChromeBlock `yaml:"preview_chrome,omitempty"`
	DefaultView         WorkspaceView       `yaml:"default_view,omitempty"`
	HiddenViews         []WorkspaceView     `yaml:"hidden_views,omitempty"`
}

type PreviewChromeBlock struct {
	Enabled        *bool      `yaml:"enabled,omitempty"`
	Refresh        *bool      `yaml:"refresh,omitempty"`
	OpenInNewTab   *bool      `yaml:"open_in_new_tab,omitempty"`
	ViewportToggle bool       `yaml:"viewport_toggle,omitempty"`
	URLBar         URLBarMode `yaml:"url_bar,omitempty"`
}

type ChatThinkingBlock struct {
	Visible          *bool `yaml:"visible,omitempty"`
	CollapsedDefault *bool `yaml:"collapsed_default,omitempty"`
}

type ChatToolCallsBlock struct {
	CollapsedDefault *bool                `yaml:"collapsed_default,omitempty"`
	ShowSilent       bool                 `yaml:"show_silent,omitempty"`
	InjectIntent     bool                 `yaml:"inject_intent,omitempty"`
	HideDetails      bool                 `yaml:"hide_details,omitempty"`
	StrictMode       bool                 `yaml:"strict_mode,omitempty"`
	IntentPhrases    *IntentPhrasesConfig `yaml:"intent_phrases,omitempty"`
}

type IntentPhrasesConfig struct {
	Source IntentPhrasesSource        `yaml:"source,omitempty"`
	LLM    *IntentPhrasesLLMConfig    `yaml:"llm,omitempty"`
	Static *IntentPhrasesStaticConfig `yaml:"static,omitempty"`
}

type IntentPhrasesLLMConfig struct {
	GatewayModel   string  `yaml:"gateway_model,omitempty"`
	MaxPhrases     int     `yaml:"max_phrases,omitempty"`
	MinPhrases     int     `yaml:"min_phrases,omitempty"`
	TimeoutSeconds float64 `yaml:"timeout_seconds,omitempty"`
	Prompt         string  `yaml:"prompt,omitempty"`
}

type IntentPhrasesStaticConfig struct {
	Phases map[string][]string `yaml:"phases,omitempty"`
}

type ChatComposerBlock struct {
	FileUpload          *bool `yaml:"file_upload,omitempty"`
	Voice               *bool `yaml:"voice,omitempty"`
	SlashCommands       *bool `yaml:"slash_commands,omitempty"`
	QuickPromptsVisible *bool `yaml:"quick_prompts_visible,omitempty"`
}

type ChatVisualBlock struct {
	Accent              string          `yaml:"accent,omitempty"`
	BubbleStyle         BubbleStyle     `yaml:"bubble_style,omitempty"`
	UserBubbleAlignment BubbleAlignment `yaml:"user_bubble_alignment,omitempty"`
}

type ActivityPanelBlock struct {
	Enabled         *bool    `yaml:"enabled,omitempty"`
	Position        Position `yaml:"position,omitempty"`
	Title           string   `yaml:"title,omitempty"`
	ShowRunning     *bool    `yaml:"show_running,omitempty"`
	ShowRecent      *bool    `yaml:"show_recent,omitempty"`
	ShowStats       *bool    `yaml:"show_stats,omitempty"`
	ShowBgTasks     *bool    `yaml:"show_bg_tasks,omitempty"`
	MaxRecent       int      `yaml:"max_recent,omitempty"`
	AutoOpenOnSpawn bool     `yaml:"auto_open_on_spawn,omitempty"`
}

type SlotsConfig struct {
	Header       *SlotEntry `yaml:"header,omitempty"`
	SidebarLeft  *SlotEntry `yaml:"sidebar_left,omitempty"`
	SidebarRight *SlotEntry `yaml:"sidebar_right,omitempty"`
	FooterLeft   *SlotEntry `yaml:"footer_left,omitempty"`
	FooterRight  *SlotEntry `yaml:"footer_right,omitempty"`
}

type SlotEntry struct {
	Kind string `yaml:"kind,omitempty"`
	Ref  string `yaml:"ref,omitempty"`
}

type WidgetsConfig struct {
	Version       int                       `yaml:"version,omitempty"`
	ChatSide      map[string]any            `yaml:"chat_side,omitempty"`
	WorkspaceTabs []map[string]any          `yaml:"workspace_tabs,omitempty"`
	Modals        map[string]map[string]any `yaml:"modals,omitempty"`
	Inline        map[string]map[string]any `yaml:"inline,omitempty"`
}
