package sessionstore

import (
	"time"
)

type EventType string

const (
	EventUserMessage      EventType = "user_message"
	EventAssistantMessage EventType = "assistant_message"
	EventSystemMessage    EventType = "system_message"
	EventMessageStarted   EventType = "message_started"
	EventMessageDone      EventType = "message_done"
	// Durable message queue. Names match what the web client already listens
	// for (stores/queue.ts) — the contract was designed, never implemented.
	EventMessageQueued    EventType = "message_queued"
	EventMessageCancelled EventType = "message_cancelled"
	EventQueueCleared     EventType = "queue_cleared"
	EventToolCall         EventType = "tool_call"
	EventToolResult       EventType = "tool_result"
	EventToolProgress     EventType = "tool_progress"
	EventApprovalRequest  EventType = "approval_request"
	EventApprovalGranted  EventType = "approval_granted"
	EventApprovalDenied   EventType = "approval_denied"
	EventToolAllowed      EventType = "tool_allowed"
	EventMemoryRemember   EventType = "memory_remember"
	EventMemoryFactAdded  EventType = "memory_fact_added"
	EventWorkspaceWrite   EventType = "workspace_write"
	EventWorkspaceEdit    EventType = "workspace_edit"
	EventWorkspaceDelete  EventType = "workspace_delete"
	EventWorkspaceChanges EventType = "workspace_changes"
	EventAgentSpawn       EventType = "agent_spawn"
	EventAgentProgress    EventType = "agent_progress"
	EventAgentResult      EventType = "agent_result"
	EventWidget           EventType = "widget"
	EventPreview          EventType = "preview"
	EventTodoAdded        EventType = "todo_added"
	EventTodoUpdated      EventType = "todo_updated"
	EventGoalSet          EventType = "goal_set"
	EventCostUpdate       EventType = "cost_update"
	EventTokenUsage       EventType = "token_usage"
	EventSessionStarted   EventType = "session_started"
	EventSessionEnded     EventType = "session_ended"
	EventSessionInterrupt EventType = "session_interrupted"
	EventSessionRenamed   EventType = "session_renamed"
	EventModelChanged     EventType = "model_changed"
	EventCompactDone      EventType = "compact_done"
	EventQuarantine       EventType = "quarantine"
	EventError            EventType = "error"

	EventContextCompacting EventType = "context_compacting"

	EventContextCompacted EventType = "context_compacted"

	EventContextTokens EventType = "context_tokens"

	// EventTurnState is a TRANSIENT (never durable) join-time signal telling a
	// (re)connecting client that a turn is currently in flight, so it can re-arm
	// its spinner. Cleared client-side by the live turn_ended/turn_terminal.
	EventTurnState EventType = "turn_state"

	// EventNotification is a TRANSIENT, USER-ROOM-only signal (never durable,
	// never sent to a session room) — cross-session alerts like "an agent needs
	// your approval in another session". Fanned out in the socket bridge to
	// user:<owner> so every tab of that user sees it regardless of which session
	// is open. Isolated by the JWT-derived user room.
	EventNotification EventType = "notification"

	EventContextSummaryPrepared EventType = "context_summary_prepared"

	EventTurnStarted      EventType = "turn_started"
	EventTurnPhaseChanged EventType = "turn_phase_changed"
	EventTurnEnded        EventType = "turn_ended"

	EventTurnRetry EventType = "turn_retry"

	EventAssistantDelta EventType = "assistant_delta"

	EventAssistantReasoningDelta EventType = "assistant_reasoning_delta"

	EventSecurityDecision EventType = "security_decision"

	EventBackgroundTask EventType = "background_task"

	EventFlowStarted   EventType = "flow_started"
	EventFlowNodeStart EventType = "flow_node_started"
	EventFlowNodeEnd   EventType = "flow_node_ended"
	EventFlowEnded     EventType = "flow_ended"
)

type Event struct {
	Seq           uint64    `json:"seq"`
	Type          EventType `json:"type"`
	TsUnixNano    int64     `json:"ts"`
	SessionID     string    `json:"session_id"`
	AppID         string    `json:"app_id,omitempty"`
	UserID        string    `json:"user_id,omitempty"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	StepID        string    `json:"step_id,omitempty"`

	LiveOutputTokens int `json:"live_output_tokens,omitempty"`

	CtxTokens *ContextTokensPayload `json:"ctx_tokens,omitempty"`

	Message          *MessagePayload          `json:"message,omitempty"`
	Tool             *ToolPayload             `json:"tool,omitempty"`
	Approval         *ApprovalPayload         `json:"approval,omitempty"`
	Notification     *NotificationPayload     `json:"notification,omitempty"`
	Memory           *MemoryPayload           `json:"memory,omitempty"`
	Workspace        *WorkspacePayload        `json:"workspace,omitempty"`
	Agent            *AgentPayload            `json:"agent,omitempty"`
	Widget           *WidgetPayload           `json:"widget,omitempty"`
	Preview          *PreviewPayload          `json:"preview,omitempty"`
	Todo             *TodoPayload             `json:"todo,omitempty"`
	Queue            *QueuePayload            `json:"queue,omitempty"`
	Cost             *CostPayload             `json:"cost,omitempty"`
	Compact          *CompactPayload          `json:"compact,omitempty"`
	CtxCompact       *ContextCompactPayload   `json:"ctx_compact,omitempty"`
	CtxSummary       *ContextSummaryPayload   `json:"ctx_summary,omitempty"`
	Meta             *MetaPayload             `json:"meta,omitempty"`
	Error            *ErrorPayload            `json:"error,omitempty"`
	Turn             *TurnPayload             `json:"turn,omitempty"`
	Retry            *RetryPayload            `json:"retry,omitempty"`
	Security         *SecurityDecisionPayload `json:"security,omitempty"`
	Background       *BackgroundTaskPayload   `json:"background,omitempty"`
	Flow             *FlowPayload             `json:"flow,omitempty"`
	WorkspaceChanges *WorkspaceChangesPayload `json:"workspace_changes,omitempty"`
	Allowed          *AllowedToolPayload      `json:"allowed,omitempty"`
}

type AllowedToolPayload struct {
	Signature string `json:"signature"`
}

type RetryPayload struct {
	Attempt   int    `json:"attempt"`
	Max       int    `json:"max"`
	Message   string `json:"message"`
	Code      string `json:"code,omitempty"`
	Category  string `json:"category,omitempty"`
	RetryInMs int    `json:"retry_in_ms"`
}

type WorkspaceChangesPayload struct {
	Files []WorkspaceChangedFile `json:"files"`
	Count int                    `json:"count"`
}

type WorkspaceChangedFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

type BackgroundTaskPayload struct {
	TaskID        string `json:"task_id"`
	Tool          string `json:"tool"`
	Label         string `json:"label,omitempty"`
	State         string `json:"state"`
	Error         string `json:"error,omitempty"`
	ElapsedMs     int64  `json:"elapsed_ms,omitempty"`
	StartedAtUnix int64  `json:"started_at_unix,omitempty"`
}

type SecurityDecisionPayload struct {
	AppID          string         `json:"app_id,omitempty"`
	AgentID        string         `json:"agent_id,omitempty"`
	SessionID      string         `json:"session_id,omitempty"`
	UserID         string         `json:"user_id,omitempty"`
	Module         string         `json:"module"`
	Action         string         `json:"action"`
	RiskLevel      string         `json:"risk_level,omitempty"`
	ParamsRedacted map[string]any `json:"params,omitempty"`
	Decision       string         `json:"decision"`
	Gate           string         `json:"gate"`
	Reason         string         `json:"reason,omitempty"`
	Caller         string         `json:"caller"`
}

type MessagePayload struct {
	Role  string        `json:"role"`
	Parts []MessagePart `json:"parts,omitempty"`

	Reasoning          string `json:"reasoning,omitempty"`
	ReasoningStartedAt int64  `json:"reasoning_started_at,omitempty"`
	ReasoningEndedAt   int64  `json:"reasoning_ended_at,omitempty"`

	ClientMessageID string `json:"client_message_id,omitempty"`

	Content     string         `json:"content,omitempty"`
	ToolCallIDs []string       `json:"tool_call_ids,omitempty"`
	Attachments []BlobRef      `json:"attachments,omitempty"`
	Extra       map[string]any `json:"extra,omitempty"`

	TriggerEvent map[string]any `json:"trigger_event,omitempty"`
}

type MessagePart struct {
	Type string `json:"type"`

	Text       string          `json:"text,omitempty"`
	Blob       *BlobRef        `json:"blob,omitempty"`
	ToolCall   *ToolCallSpec   `json:"tool_call,omitempty"`
	ToolResult *ToolResultSpec `json:"tool_result,omitempty"`

	URL       string `json:"url,omitempty"`
	Thumbnail string `json:"thumbnail,omitempty"`
}

const (
	PartTypeText       = "text"
	PartTypeImage      = "image"
	PartTypeAudio      = "audio"
	PartTypeVideo      = "video"
	PartTypeFile       = "file"
	PartTypeToolCall   = "tool_call"
	PartTypeToolResult = "tool_result"
)

type ToolCallSpec struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type ToolResultSpec struct {
	ToolCallID string        `json:"tool_call_id"`
	Parts      []MessagePart `json:"parts,omitempty"`
	Error      string        `json:"error,omitempty"`
}

type ToolPayload struct {
	CallID          string         `json:"call_id"`
	Name            string         `json:"name"`
	Arguments       map[string]any `json:"arguments,omitempty"`
	Status          string         `json:"status,omitempty"`
	LiveTokens      int            `json:"live_tokens,omitempty"`
	Detail          string         `json:"detail,omitempty"`
	Output          any            `json:"output,omitempty"`
	Parts           []MessagePart  `json:"parts,omitempty"`
	Error           string         `json:"error,omitempty"`
	DurationMs      int64          `json:"duration_ms,omitempty"`
	Diff            string         `json:"diff,omitempty"`
	UnifiedDiff     string         `json:"unified_diff,omitempty"`
	PreviousContent string         `json:"previous_content,omitempty"`
	NewContent      string         `json:"new_content,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
}

type ApprovalPayload struct {
	ID      string         `json:"id"`
	Kind    string         `json:"kind"`
	Payload map[string]any `json:"payload,omitempty"`
	Status  string         `json:"status,omitempty"`
	Reason  string         `json:"reason,omitempty"`

	AgentID    string         `json:"agent_id,omitempty"`
	ToolName   string         `json:"tool_name,omitempty"`
	ToolParams map[string]any `json:"tool_params,omitempty"`
	RiskLevel  string         `json:"risk_level,omitempty"`
	CallID     string         `json:"call_id,omitempty"`
}

// NotificationPayload is the body of an EventNotification (user-room only).
// Kind: "approval_pending" (an agent is waiting on the user) or
// "approval_cleared" (that approval resolved). SessionID/AppID let the client
// route the user to the right session; Title is a short preview (the question).
type NotificationPayload struct {
	Kind      string `json:"kind"`
	SessionID string `json:"session_id"`
	AppID     string `json:"app_id,omitempty"`
	Title     string `json:"title,omitempty"`
}

type MemoryPayload struct {
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`
	Scope string `json:"scope,omitempty"`
	Fact  string `json:"fact,omitempty"`
}

type WorkspacePayload struct {
	Path         string `json:"path"`
	ContentHash  string `json:"content_hash,omitempty"`
	BaselineHash string `json:"baseline_hash,omitempty"`
	Bytes        int64  `json:"bytes,omitempty"`
	Operation    string `json:"operation,omitempty"`
}

type AgentPayload struct {
	RunID          string `json:"run_id"`
	ParentRunID    string `json:"parent_run_id,omitempty"`
	ParentCallID   string `json:"parent_call_id,omitempty"`
	Kind           string `json:"kind,omitempty"`
	ChildSessionID string `json:"child_session_id,omitempty"`
	Status         string `json:"status,omitempty"`
	ResultSummary  string `json:"result_summary,omitempty"`
	Depth          int    `json:"depth,omitempty"`
	Fork           bool   `json:"fork,omitempty"`

	ToolCalls   int64  `json:"tool_calls,omitempty"`
	LLMCalls    int64  `json:"llm_calls,omitempty"`
	TokensIn    int64  `json:"tokens_in,omitempty"`
	TokensOut   int64  `json:"tokens_out,omitempty"`
	Children    int64  `json:"children,omitempty"`
	DurationMs  int64  `json:"duration_ms,omitempty"`
	CurrentTool string `json:"current_tool,omitempty"`
}

type WidgetPayload struct {
	ID    string         `json:"id"`
	Kind  string         `json:"kind"`
	State map[string]any `json:"state,omitempty"`
}

type PreviewPayload struct {
	ID      string         `json:"id"`
	URL     string         `json:"url,omitempty"`
	Status  string         `json:"status,omitempty"`
	Payload map[string]any `json:"payload,omitempty"`
}

// QueuePayload carries one durable queue row. Field names are the contract the
// web client parses in models/queue-entry.ts (queueEntryFromJson) — keep them
// in sync or the panel silently renders empty rows.
type QueuePayload struct {
	ID            string `json:"id"`
	CorrelationID string `json:"correlation_id,omitempty"`
	Message       string `json:"message,omitempty"`
	Status        string `json:"status,omitempty"`
	Position      int    `json:"position,omitempty"`
	EnqueuedAt    int64  `json:"enqueued_at,omitempty"`
	StartedAt     int64  `json:"started_at,omitempty"`
	FinishedAt    int64  `json:"finished_at,omitempty"`
	ErrorCode     string `json:"error_code,omitempty"`
	// Depth/Max ride the queue_full signal only.
	Depth int `json:"depth,omitempty"`
	Max   int `json:"max,omitempty"`
}

type TodoPayload struct {
	ID     string `json:"id"`
	Text   string `json:"text,omitempty"`
	Status string `json:"status,omitempty"`
}

type CostPayload struct {
	TokensIn         int64   `json:"tokens_in,omitempty"`
	TokensOut        int64   `json:"tokens_out,omitempty"`
	ReasoningTokens  int64   `json:"reasoning_tokens,omitempty"`
	UsdTotal         float64 `json:"usd_total,omitempty"`
	CacheReadTokens  int64   `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64   `json:"cache_write_tokens,omitempty"`
}

type CompactPayload struct {
	CutoffSeq      uint64 `json:"cutoff_seq"`
	SnapshotSHA256 string `json:"snapshot_sha256"`
	Binary         bool   `json:"binary,omitempty"`
	EventsBefore   int    `json:"events_before"`
	BytesBefore    int64  `json:"bytes_before"`
	DurationMs     int64  `json:"duration_ms"`
}

type ContextCompactPayload struct {
	CutoffSeq        uint64 `json:"cutoff_seq"`
	Summary          string `json:"summary,omitempty"`
	KeepRecent       int    `json:"keep_recent,omitempty"`
	Strategy         string `json:"strategy,omitempty"`
	MessagesDropped  int    `json:"messages_dropped,omitempty"`
	TokensBefore     int    `json:"tokens_before,omitempty"`
	TokensFreed      int    `json:"tokens_freed,omitempty"`
	NewContextTokens int    `json:"new_context_tokens,omitempty"`
}

type ContextTokensPayload struct {
	Total    int `json:"total"`
	System   int `json:"system,omitempty"`
	Tools    int `json:"tools,omitempty"`
	Messages int `json:"messages,omitempty"`
	Window   int `json:"window,omitempty"`
	Limit    int `json:"limit,omitempty"`
}

type ContextSummaryPayload struct {
	Summary     string `json:"summary"`
	CoversSeq   uint64 `json:"covers_seq"`
	InputTokens int    `json:"input_tokens,omitempty"`
	Model       string `json:"model,omitempty"`
}

type MetaPayload struct {
	Title            string `json:"title,omitempty"`
	Workspace        string `json:"workspace,omitempty"`
	Workdir          string `json:"workdir,omitempty"`
	Interrupted      bool   `json:"interrupted,omitempty"`
	Model            string `json:"model,omitempty"`
	AgentID          string `json:"agent_id,omitempty"`
	MaxContextTokens int    `json:"max_ctx_tokens,omitempty"`
	ReasoningEffort  string `json:"reasoning_effort,omitempty"`
	Provider         string `json:"provider,omitempty"`
	MaxOutputTokens  int    `json:"max_output_tokens,omitempty"`
	EntryAgent       string `json:"entry_agent,omitempty"`
	ContextExtra     string `json:"context,omitempty"`
	Actor            string `json:"actor,omitempty"`
}

type ErrorPayload struct {
	Code     string         `json:"code,omitempty"`
	Message  string         `json:"message"`
	Error    string         `json:"error,omitempty"`
	Category string         `json:"category,omitempty"`
	Detail   string         `json:"detail,omitempty"`
	Retry    *bool          `json:"retry,omitempty"`
	Subcode  string         `json:"subcode,omitempty"`
	Source   string         `json:"source,omitempty"`
	Fatal    bool           `json:"fatal,omitempty"`
	Stack    string         `json:"stack,omitempty"`
	Context  map[string]any `json:"context,omitempty"`
}

type BlobRef struct {
	Hash string `json:"hash"`
	Mime string `json:"mime"`
	Size int64  `json:"size"`
	Name string `json:"name,omitempty"`
}

type TurnPayload struct {
	TurnID  string `json:"turn_id"`
	AgentID string `json:"agent_id,omitempty"`
	Phase   string `json:"phase,omitempty"`
	Status  string `json:"status,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

func (e Event) Time() time.Time { return time.Unix(0, e.TsUnixNano).UTC() }

type FlowPayload struct {
	FlowID    string `json:"flow_id,omitempty"`
	NodeID    string `json:"node_id,omitempty"`
	NodeType  string `json:"node_type,omitempty"`
	Status    string `json:"status,omitempty"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
	Iteration int    `json:"iteration,omitempty"`
}
