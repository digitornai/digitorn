// Package schema mirrors the Pydantic AppDefinition with Go-typed structs.
package schema

type Mode string

const (
	ModeConversation Mode = "conversation"
	ModeOneShot      Mode = "one_shot"
	ModeBackground   Mode = "background"
	ModePipeline     Mode = "pipeline"
)

var AllModes = []Mode{ModeConversation, ModeOneShot, ModeBackground, ModePipeline}

type SessionMode string

const (
	SessionModeMono  SessionMode = "mono"
	SessionModeMulti SessionMode = "multi"
)

var AllSessionModes = []SessionMode{SessionModeMono, SessionModeMulti}

type WorkdirMode string

const (
	WorkdirNone     WorkdirMode = "none"
	WorkdirRequired WorkdirMode = "required"
	WorkdirFixed    WorkdirMode = "fixed"
	WorkdirAuto     WorkdirMode = "auto"
)

var AllWorkdirModes = []WorkdirMode{WorkdirNone, WorkdirRequired, WorkdirFixed, WorkdirAuto}

type ToolInjection string

const (
	ToolInjectionDirect        ToolInjection = "direct"
	ToolInjectionCompactDirect ToolInjection = "compact_direct"
	ToolInjectionDiscovery     ToolInjection = "discovery"
)

var AllToolInjections = []ToolInjection{ToolInjectionDirect, ToolInjectionCompactDirect, ToolInjectionDiscovery}

type ContextStrategy string

const (
	ContextStrategyTruncate  ContextStrategy = "truncate"
	ContextStrategySummarize ContextStrategy = "summarize"
)

var AllContextStrategies = []ContextStrategy{ContextStrategyTruncate, ContextStrategySummarize}

type AttachmentsMode string

const (
	AttachmentsModeDirect AttachmentsMode = "direct"
	AttachmentsModeTool   AttachmentsMode = "tool"
)

var AllAttachmentsModes = []AttachmentsMode{AttachmentsModeDirect, AttachmentsModeTool}

type AttachmentType string

const (
	AttachmentImage    AttachmentType = "image"
	AttachmentDocument AttachmentType = "document"
	AttachmentAudio    AttachmentType = "audio"
	AttachmentVideo    AttachmentType = "video"
)

var AllAttachmentTypes = []AttachmentType{AttachmentImage, AttachmentDocument, AttachmentAudio, AttachmentVideo}

type Category string

const (
	CategoryCoding        Category = "coding"
	CategoryWriting       Category = "writing"
	CategoryResearch      Category = "research"
	CategoryData          Category = "data"
	CategoryDevOps        Category = "devops"
	CategoryDesign        Category = "design"
	CategoryCommunication Category = "communication"
	CategoryAutomation    Category = "automation"
	CategoryGeneral       Category = "general"
)

var AllCategories = []Category{
	CategoryCoding, CategoryWriting, CategoryResearch, CategoryData,
	CategoryDevOps, CategoryDesign, CategoryCommunication, CategoryAutomation,
	CategoryGeneral,
	// Hub-canonical ids (GET /api/v1/categories). "assistant" + "creative" were
	// missing, so a chat app couldn't declare its real category and fell back to
	// the ugly uppercased id in the store.
	"developer-tools", "development", "productivity", "education", "ai", "support",
	"assistant", "creative",
}

type ClassifierFrequency string

const (
	ClassifierEveryTurn    ClassifierFrequency = "every_turn"
	ClassifierFirstTurn    ClassifierFrequency = "first_turn"
	ClassifierEveryNTurns  ClassifierFrequency = "every_n_turns"
	ClassifierOnNewMessage ClassifierFrequency = "on_new_message"
)

var AllClassifierFrequencies = []ClassifierFrequency{
	ClassifierEveryTurn, ClassifierFirstTurn, ClassifierEveryNTurns, ClassifierOnNewMessage,
}

type TriggerType string

const (
	TriggerCron  TriggerType = "cron"
	TriggerWatch TriggerType = "watch"
	TriggerHTTP  TriggerType = "http"
)

var AllTriggerTypes = []TriggerType{TriggerCron, TriggerWatch, TriggerHTTP}

type HTTPMethod string

const (
	MethodGET     HTTPMethod = "GET"
	MethodPOST    HTTPMethod = "POST"
	MethodPUT     HTTPMethod = "PUT"
	MethodDELETE  HTTPMethod = "DELETE"
	MethodPATCH   HTTPMethod = "PATCH"
	MethodHEAD    HTTPMethod = "HEAD"
	MethodOPTIONS HTTPMethod = "OPTIONS"
)

var AllHTTPMethods = []HTTPMethod{MethodGET, MethodPOST, MethodPUT, MethodDELETE, MethodPATCH, MethodHEAD, MethodOPTIONS}

type Routing string

const (
	RoutingBroadcast Routing = "broadcast"
	RoutingUser      Routing = "user"
	RoutingSession   Routing = "session"
)

var AllRoutings = []Routing{RoutingBroadcast, RoutingUser, RoutingSession}

type CapabilityPolicy string

const (
	CapAuto    CapabilityPolicy = "auto"
	CapApprove CapabilityPolicy = "approve"
	CapBlock   CapabilityPolicy = "block"
)

var AllCapabilityPolicies = []CapabilityPolicy{CapAuto, CapApprove, CapBlock, "grant", "deny", "hidden"}

type RiskLevel string

const (
	RiskLow    RiskLevel = "low"
	RiskMedium RiskLevel = "medium"
	RiskHigh   RiskLevel = "high"
)

var AllRiskLevels = []RiskLevel{RiskLow, RiskMedium, RiskHigh}

type SandboxLevel string

const (
	SandboxOff      SandboxLevel = "off"
	SandboxStandard SandboxLevel = "standard"
	SandboxStrict   SandboxLevel = "strict"
	SandboxMaximum  SandboxLevel = "maximum"
)

var AllSandboxLevels = []SandboxLevel{SandboxOff, SandboxStandard, SandboxStrict, SandboxMaximum}

type CredentialScope string

const (
	CredScopePerUser      CredentialScope = "per_user"
	CredScopePerAppShared CredentialScope = "per_app_shared"
	CredScopeSystemWide   CredentialScope = "system_wide"
)

var AllCredentialScopes = []CredentialScope{CredScopePerUser, CredScopePerAppShared, CredScopeSystemWide}

type CredentialType string

const (
	CredTypeAPIKey           CredentialType = "api_key"
	CredTypeMultiField       CredentialType = "multi_field"
	CredTypeOAuth2           CredentialType = "oauth2"
	CredTypeConnectionString CredentialType = "connection_string"
	CredTypeMCPServer        CredentialType = "mcp_server"
	CredTypeCustom           CredentialType = "custom"
)

var AllCredentialTypes = []CredentialType{
	CredTypeAPIKey, CredTypeMultiField, CredTypeOAuth2,
	CredTypeConnectionString, CredTypeMCPServer, CredTypeCustom,
}

type CredentialFieldType string

const (
	CredFieldSecret           CredentialFieldType = "secret"
	CredFieldString           CredentialFieldType = "string"
	CredFieldURL              CredentialFieldType = "url"
	CredFieldSelect           CredentialFieldType = "select"
	CredFieldNumber           CredentialFieldType = "number"
	CredFieldBoolean          CredentialFieldType = "boolean"
	CredFieldConnectionString CredentialFieldType = "connection_string"
)

var AllCredentialFieldTypes = []CredentialFieldType{
	CredFieldSecret, CredFieldString, CredFieldURL, CredFieldSelect,
	CredFieldNumber, CredFieldBoolean, CredFieldConnectionString,
}

type InputType string

const (
	InputText  InputType = "text"
	InputImage InputType = "image"
	InputAudio InputType = "audio"
	InputVideo InputType = "video"
	InputFile  InputType = "file"
	InputJSON  InputType = "json"
	InputAny   InputType = "any"
)

var AllInputTypes = []InputType{InputText, InputImage, InputAudio, InputVideo, InputFile, InputJSON, InputAny}

type OutputType string

const (
	OutputText     OutputType = "text"
	OutputJSON     OutputType = "json"
	OutputMarkdown OutputType = "markdown"
	OutputFile     OutputType = "file"
	OutputImage    OutputType = "image"
	OutputAudio    OutputType = "audio"
)

var AllOutputTypes = []OutputType{OutputText, OutputJSON, OutputMarkdown, OutputFile, OutputImage, OutputAudio}

type Backend string

const (
	BackendOpenAICompat  Backend = "openai_compat"
	BackendAnthropic     Backend = "anthropic"
	BackendGitHubCopilot Backend = "github_copilot"
)

var AllBackends = []Backend{BackendOpenAICompat, BackendAnthropic, BackendGitHubCopilot}

type ImageDetail string

const (
	ImageDetailAuto ImageDetail = "auto"
	ImageDetailLow  ImageDetail = "low"
	ImageDetailHigh ImageDetail = "high"
)

var AllImageDetails = []ImageDetail{ImageDetailAuto, ImageDetailLow, ImageDetailHigh}

type Provider string

const (
	ProviderAnthropic     Provider = "anthropic"
	ProviderOpenAI        Provider = "openai"
	ProviderDeepSeek      Provider = "deepseek"
	ProviderGroq          Provider = "groq"
	ProviderMistral       Provider = "mistral"
	ProviderTogether      Provider = "together"
	ProviderOllama        Provider = "ollama"
	ProviderLMStudio      Provider = "lm_studio"
	ProviderVLLM          Provider = "vllm"
	ProviderGoogleGemini  Provider = "google-gemini"
	ProviderGemini        Provider = "gemini"
	ProviderXAI           Provider = "xai"
	ProviderGrok          Provider = "grok"
	ProviderCerebras      Provider = "cerebras"
	ProviderPerplexity    Provider = "perplexity"
	ProviderFireworks     Provider = "fireworks"
	ProviderGitHubCopilot Provider = "github_copilot"
)

var KnownProviders = []Provider{
	ProviderAnthropic, ProviderOpenAI, ProviderDeepSeek, ProviderGroq,
	ProviderMistral, ProviderTogether, ProviderOllama, ProviderLMStudio,
	ProviderVLLM, ProviderGoogleGemini, ProviderGemini, ProviderXAI,
	ProviderGrok, ProviderCerebras, ProviderPerplexity, ProviderFireworks,
	ProviderGitHubCopilot,
}

var LocalProviders = map[Provider]struct{}{
	ProviderOllama:   {},
	ProviderLMStudio: {},
	ProviderVLLM:     {},
}

type RuleWhen string

const (
	RuleWhenPreTool  RuleWhen = "pre_tool"
	RuleWhenPostTool RuleWhen = "post_tool"
	RuleWhenOnText   RuleWhen = "on_text"
)

var AllRuleWhens = []RuleWhen{RuleWhenPreTool, RuleWhenPostTool, RuleWhenOnText}

type RuleAction string

const (
	RuleActionBlock  RuleAction = "block"
	RuleActionWarn   RuleAction = "warn"
	RuleActionRemind RuleAction = "remind"
)

var AllRuleActions = []RuleAction{RuleActionBlock, RuleActionWarn, RuleActionRemind}

type Layout string

const (
	LayoutDefault  Layout = "default"
	LayoutCode     Layout = "code"
	LayoutBuilder  Layout = "builder"
	LayoutResearch Layout = "research"
	LayoutMinimal  Layout = "minimal"
	LayoutLovable  Layout = "lovable"
)

var AllLayouts = []Layout{LayoutDefault, LayoutCode, LayoutBuilder, LayoutResearch, LayoutMinimal, LayoutLovable}

type Density string

const (
	DensityCompact     Density = "compact"
	DensityComfortable Density = "comfortable"
)

var AllDensities = []Density{DensityCompact, DensityComfortable}

type Position string

const (
	PositionRight   Position = "right"
	PositionBottom  Position = "bottom"
	PositionHidden  Position = "hidden"
	PositionOverlay Position = "overlay"
)

var AllPositions = []Position{PositionRight, PositionBottom, PositionHidden, PositionOverlay}

type RenderMode string

const (
	RenderReact    RenderMode = "react"
	RenderHTML     RenderMode = "html"
	RenderMarkdown RenderMode = "markdown"
	RenderSlides   RenderMode = "slides"
	RenderCode     RenderMode = "code"
	RenderLatex    RenderMode = "latex"
	RenderBuilder  RenderMode = "builder"
	RenderAuto     RenderMode = "auto"
)

var AllRenderModes = []RenderMode{
	RenderReact, RenderHTML, RenderMarkdown, RenderSlides,
	RenderCode, RenderLatex, RenderBuilder, RenderAuto,
}

type WorkspaceView string

const (
	ViewCode      WorkspaceView = "code"
	ViewPreview   WorkspaceView = "preview"
	ViewChanges   WorkspaceView = "changes"
	ViewActivity  WorkspaceView = "activity"
	ViewDocuments WorkspaceView = "documents"
	ViewAuto      WorkspaceView = "auto"
)

var AllWorkspaceViews = []WorkspaceView{ViewCode, ViewPreview, ViewChanges, ViewActivity, ViewDocuments, ViewAuto}

type BubbleStyle string

const (
	BubbleCard    BubbleStyle = "card"
	BubbleFlat    BubbleStyle = "flat"
	BubbleMinimal BubbleStyle = "minimal"
)

var AllBubbleStyles = []BubbleStyle{BubbleCard, BubbleFlat, BubbleMinimal}

type BubbleAlignment string

const (
	BubbleAlignRight BubbleAlignment = "right"
	BubbleAlignLeft  BubbleAlignment = "left"
)

var AllBubbleAlignments = []BubbleAlignment{BubbleAlignRight, BubbleAlignLeft}

type URLBarMode string

const (
	URLBarAuto   URLBarMode = "auto"
	URLBarAlways URLBarMode = "always"
	URLBarNever  URLBarMode = "never"
)

var AllURLBarModes = []URLBarMode{URLBarAuto, URLBarAlways, URLBarNever}

type MCPTransport string

const (
	MCPTransportStdio          MCPTransport = "stdio"
	MCPTransportSSE            MCPTransport = "sse"
	MCPTransportStreamableHTTP MCPTransport = "streamable_http"
	MCPTransportHTTP           MCPTransport = "http"
)

var AllMCPTransports = []MCPTransport{MCPTransportStdio, MCPTransportSSE, MCPTransportStreamableHTTP, MCPTransportHTTP}

type IntentPhrasesSource string

const (
	IntentSourceLLM    IntentPhrasesSource = "llm"
	IntentSourceStatic IntentPhrasesSource = "static"
	IntentSourceAuto   IntentPhrasesSource = "auto"
)

var AllIntentPhrasesSources = []IntentPhrasesSource{IntentSourceLLM, IntentSourceStatic, IntentSourceAuto}

type Role string

const (
	RoleWorker      Role = "worker"
	RoleCoordinator Role = "coordinator"
	RoleSpecialist  Role = "specialist"
	RoleAssistant   Role = "assistant"
)

var KnownRoles = []Role{RoleWorker, RoleCoordinator, RoleSpecialist, RoleAssistant}

var KnownUIFeatures = []string{
	"voice", "attachments", "tools_panel", "snippets", "tasks_panel",
	"memory_panel", "context_ring", "markdown", "slash_commands",
	"message_actions", "status_pills", "token_badges",
}

var KnownModeIcons = []string{"lightbulb", "map", "sparkles", "wrench", "shield"}

var KnownModeAccents = []string{"primary", "secondary", "cyan", "purple", "red", "green", "orange"}

type PayloadFieldType string

const (
	PayloadFieldString  PayloadFieldType = "string"
	PayloadFieldNumber  PayloadFieldType = "number"
	PayloadFieldInteger PayloadFieldType = "integer"
	PayloadFieldBoolean PayloadFieldType = "boolean"
	PayloadFieldSelect  PayloadFieldType = "select"
	PayloadFieldText    PayloadFieldType = "text"
)

var AllPayloadFieldTypes = []PayloadFieldType{
	PayloadFieldString, PayloadFieldNumber, PayloadFieldInteger,
	PayloadFieldBoolean, PayloadFieldSelect, PayloadFieldText,
}

// MidTurnMode is how a session treats a user message that arrives mid-turn.
type MidTurnMode string

const (
	// MidTurnQueue holds the message until the running turn ends.
	MidTurnQueue MidTurnMode = "queue"
	// MidTurnInject folds it into the running turn at the next safe boundary.
	MidTurnInject MidTurnMode = "inject"
)

var AllMidTurnModes = []MidTurnMode{MidTurnQueue, MidTurnInject}

// Resolved returns the effective mode, defaulting to queue (the conservative
// choice: an unset app behaves exactly as before).
func (m MidTurnMode) Resolved() MidTurnMode {
	if m == MidTurnInject {
		return MidTurnInject
	}
	return MidTurnQueue
}
