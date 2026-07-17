package sessionstore

import (
	"sync"
	"time"
)

type SessionState struct {
	mu sync.RWMutex

	SessionID     string
	AppID         string
	UserID        string
	StartedAtNano int64
	EndedAtNano   int64
	LastEventTsNano int64

	FirstSeq uint64
	LastSeq  uint64

	Messages        []Message
	ToolCalls       map[string]*ToolCallState
	Approvals       map[string]*ApprovalState
	Memory          map[string]string
	Facts             []string
	AllowedSignatures []string
	Goal              string
	WorkspaceFiles  map[string]*FileState
	Todos           []Todo
	Children        []ChildAgent
	BackgroundTasks []BackgroundTaskState
	Widgets         map[string]*WidgetState
	Previews        map[string]*PreviewState
	Errors          []ErrorEntry
	Compactions     []CompactionEntry

	SeenClientMsgIDs map[string]uint64

	Tools []ToolSpec

	ContextCompaction *ContextCompactionState

	CompactionInflight bool

	ContextTokens int

	ContextSystemTokens  int
	ContextToolsTokens   int
	ContextMessageTokens int

	ContextProviderTokens int

	PreparedSummary *PreparedSummaryState

	TokensIn  int64
	TokensOut int64
	UsdTotal  float64

	Title        string
	Workspace    string
	Workdir      string
	EntryAgent   string
	ContextExtra string
	ModelOverrides map[string]string
	ProviderOverrides map[string]string
	OutputTokenOverrides map[string]int
	EntryModelWindow int
	ReasoningEffort  string
	EffortOverrides map[string]string
	TurnCount       int
	Interrupted      bool
	LastUserMessage string

	CurrentTurnID            string
	CurrentTurnPhase         string
	CurrentTurnStartedAtNano int64

	ActiveMode string

	Closed bool

	EventCount uint64
	BytesEst   int64
	Partial    bool
}

type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type Message struct {
	Seq         uint64        `json:"seq"`
	StepID      string        `json:"step_id,omitempty"`
	Role        string        `json:"role"`
	Parts       []MessagePart `json:"parts,omitempty"`
	Content     string        `json:"content"`
	Reasoning   string        `json:"reasoning,omitempty"`
	ReasoningStartedAt int64         `json:"reasoning_started_at,omitempty"`
	ReasoningEndedAt   int64         `json:"reasoning_ended_at,omitempty"`
	TsUnixNano  int64         `json:"ts"`
	ToolCallIDs []string      `json:"tool_call_ids,omitempty"`
	Attachments []BlobRef     `json:"attachments,omitempty"`
	TriggerEvent map[string]any `json:"trigger_event,omitempty"`
}

type ToolCallState struct {
	CallID          string         `json:"call_id"`
	Name            string         `json:"name"`
	Arguments       map[string]any `json:"arguments,omitempty"`
	Status          string         `json:"status"`
	Output          any            `json:"output,omitempty"`
	Error           string         `json:"error,omitempty"`
	StartedAt       int64          `json:"started_at,omitempty"`
	CompletedAt     int64          `json:"completed_at,omitempty"`
	StartedSeq      uint64         `json:"started_seq,omitempty"`
	CompletedSeq    uint64         `json:"completed_seq,omitempty"`
	DurationMs      int64          `json:"duration_ms,omitempty"`
	UnifiedDiff     string         `json:"unified_diff,omitempty"`
	PreviousContent string         `json:"previous_content,omitempty"`
	NewContent      string         `json:"new_content,omitempty"`
}

type ApprovalState struct {
	ID         string         `json:"id"`
	Kind       string         `json:"kind"`
	Payload    map[string]any `json:"payload,omitempty"`
	Status     string         `json:"status"`
	Reason     string         `json:"reason,omitempty"`
	CreatedAt  int64          `json:"created_at"`
	ResolvedAt int64          `json:"resolved_at,omitempty"`

	AgentID    string         `json:"agent_id,omitempty"`
	ToolName   string         `json:"tool_name,omitempty"`
	ToolParams map[string]any `json:"tool_params,omitempty"`
	RiskLevel  string         `json:"risk_level,omitempty"`
}

type FileState struct {
	Path         string `json:"path"`
	ContentHash  string `json:"content_hash"`
	BaselineHash string `json:"baseline_hash,omitempty"`
	Bytes        int64  `json:"bytes"`
	UpdatedAt    int64  `json:"updated_at"`
}

type Todo struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
}

type ChildAgent struct {
	RunID          string `json:"run_id"`
	ParentRunID    string `json:"parent_run_id,omitempty"`
	ParentCallID   string `json:"parent_call_id,omitempty"`
	Kind           string `json:"kind,omitempty"`
	ChildSessionID string `json:"child_session_id,omitempty"`
	Status         string `json:"status"`
	ResultSummary  string `json:"result_summary,omitempty"`
	Depth          int    `json:"depth,omitempty"`
	SpawnedAt      int64  `json:"spawned_at"`
	CompletedAt    int64  `json:"completed_at,omitempty"`
	UpdatedAt      int64  `json:"updated_at,omitempty"`

	ToolCalls   int64  `json:"tool_calls,omitempty"`
	LLMCalls    int64  `json:"llm_calls,omitempty"`
	TokensIn    int64  `json:"tokens_in,omitempty"`
	TokensOut   int64  `json:"tokens_out,omitempty"`
	Children    int64  `json:"children,omitempty"`
	DurationMs  int64  `json:"duration_ms,omitempty"`
	CurrentTool string `json:"current_tool,omitempty"`
}

type BackgroundTaskState struct {
	TaskID        string `json:"task_id"`
	Tool          string `json:"tool,omitempty"`
	Label         string `json:"label,omitempty"`
	State         string `json:"state"`
	Error         string `json:"error,omitempty"`
	ElapsedMs     int64  `json:"elapsed_ms,omitempty"`
	StartedAtUnix int64  `json:"started_at_unix,omitempty"`
	UpdatedAtNano int64  `json:"updated_at,omitempty"`
}

type WidgetState struct {
	ID    string         `json:"id"`
	Kind  string         `json:"kind"`
	State map[string]any `json:"state,omitempty"`
}

type PreviewState struct {
	ID      string         `json:"id"`
	URL     string         `json:"url,omitempty"`
	Status  string         `json:"status,omitempty"`
	Payload map[string]any `json:"payload,omitempty"`
}

type ErrorEntry struct {
	Seq        uint64 `json:"seq"`
	TsUnixNano int64  `json:"ts"`
	Code       string `json:"code,omitempty"`
	Message    string `json:"message"`
	Source     string `json:"source,omitempty"`
	Fatal      bool   `json:"fatal,omitempty"`
}

type CompactionEntry struct {
	Seq            uint64 `json:"seq"`
	TsUnixNano     int64  `json:"ts"`
	CutoffSeq      uint64 `json:"cutoff_seq"`
	SnapshotSHA256 string `json:"snapshot_sha256"`
	Binary         bool   `json:"binary,omitempty"`
	EventsBefore   int    `json:"events_before"`
	DurationMs     int64  `json:"duration_ms"`
}

type ContextCompactionState struct {
	CutoffSeq  uint64 `json:"cutoff_seq"`
	Summary    string `json:"summary,omitempty"`
	KeepRecent int    `json:"keep_recent,omitempty"`
	Strategy   string `json:"strategy,omitempty"`
	AtSeq      uint64 `json:"at_seq,omitempty"`
	TsUnixNano int64  `json:"ts,omitempty"`
}

func cloneContextCompaction(c *ContextCompactionState) *ContextCompactionState {
	if c == nil {
		return nil
	}
	cp := *c
	return &cp
}

type PreparedSummaryState struct {
	Summary    string `json:"summary"`
	CoversSeq  uint64 `json:"covers_seq"`
	AtSeq      uint64 `json:"at_seq,omitempty"`
	TsUnixNano int64  `json:"ts,omitempty"`
}

func clonePreparedSummary(p *PreparedSummaryState) *PreparedSummaryState {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

func NewSessionState(sessionID string) *SessionState {
	return &SessionState{
		SessionID:      sessionID,
		ToolCalls:      map[string]*ToolCallState{},
		Approvals:      map[string]*ApprovalState{},
		Memory:         map[string]string{},
		WorkspaceFiles: map[string]*FileState{},
		Widgets:        map[string]*WidgetState{},
		Previews:       map[string]*PreviewState{},
	}
}

func (s *SessionState) RLock()   { s.mu.RLock() }
func (s *SessionState) RUnlock() { s.mu.RUnlock() }

func (s *SessionState) SeenClientMessage(id string) (uint64, bool) {
	if id == "" {
		return 0, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	seq, ok := s.SeenClientMsgIDs[id]
	return seq, ok
}

func (s *SessionState) Snapshot() SessionSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := SessionSnapshot{
		Version:                  snapshotVersion,
		SessionID:                s.SessionID,
		AppID:                    s.AppID,
		UserID:                   s.UserID,
		StartedAtNano:            s.StartedAtNano,
		EndedAtNano:              s.EndedAtNano,
		FirstSeq:                 s.FirstSeq,
		LastSeq:                  s.LastSeq,
		CapturedAtNano:           time.Now().UnixNano(),
		Messages:                 append([]Message(nil), s.Messages...),
		ToolCalls:                cloneToolCalls(s.ToolCalls),
		Approvals:                cloneApprovals(s.Approvals),
		Memory:                   cloneStringMap(s.Memory),
		Facts:             append([]string(nil), s.Facts...),
		AllowedSignatures: append([]string(nil), s.AllowedSignatures...),
		Goal:              s.Goal,
		WorkspaceFiles:           cloneFileStates(s.WorkspaceFiles),
		Todos:                    append([]Todo(nil), s.Todos...),
		Children:                 append([]ChildAgent(nil), s.Children...),
		BackgroundTasks:          append([]BackgroundTaskState(nil), s.BackgroundTasks...),
		Widgets:                  cloneWidgets(s.Widgets),
		Previews:                 clonePreviews(s.Previews),
		Errors:                   append([]ErrorEntry(nil), s.Errors...),
		Compactions:              append([]CompactionEntry(nil), s.Compactions...),
		ContextCompaction:        cloneContextCompaction(s.ContextCompaction),
		PreparedSummary:          clonePreparedSummary(s.PreparedSummary),
		CompactionInflight:       s.CompactionInflight,
		ContextTokens:            s.ContextTokens,
		ContextSystemTokens:      s.ContextSystemTokens,
		ContextToolsTokens:       s.ContextToolsTokens,
		ContextMessageTokens:     s.ContextMessageTokens,
		ContextProviderTokens:    s.ContextProviderTokens,
		TokensIn:                 s.TokensIn,
		TokensOut:                s.TokensOut,
		UsdTotal:                 s.UsdTotal,
		Title:                    s.Title,
		Workspace:                s.Workspace,
		Workdir:                  s.Workdir,
			EntryAgent:               s.EntryAgent,
			ContextExtra:             s.ContextExtra,
			ModelOverrides:           s.ModelOverrides,
			ProviderOverrides:        s.ProviderOverrides,
			OutputTokenOverrides:     s.OutputTokenOverrides,
			EntryModelWindow:         s.EntryModelWindow,
			ReasoningEffort:          s.ReasoningEffort,
			EffortOverrides:          s.EffortOverrides,
		CurrentTurnID:            s.CurrentTurnID,
		CurrentTurnPhase:         s.CurrentTurnPhase,
		CurrentTurnStartedAtNano: s.CurrentTurnStartedAtNano,
		ActiveMode:               s.ActiveMode,
		Closed:                   s.Closed,
		EventCount:               s.EventCount,
		BytesEst:                 s.BytesEst,
		Partial:                  s.Partial,
		TurnCount:                s.TurnCount,
		Interrupted:              s.Interrupted,
		LastUserMessage:          s.LastUserMessage,
	}
	return snap
}

func cloneToolCalls(in map[string]*ToolCallState) map[string]*ToolCallState {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*ToolCallState, len(in))
	for k, v := range in {
		c := *v
		if v.Arguments != nil {
			c.Arguments = cloneAnyMap(v.Arguments)
		}
		out[k] = &c
	}
	return out
}

func cloneApprovals(in map[string]*ApprovalState) map[string]*ApprovalState {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*ApprovalState, len(in))
	for k, v := range in {
		c := *v
		if v.Payload != nil {
			c.Payload = cloneAnyMap(v.Payload)
		}
		out[k] = &c
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneAnyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneFileStates(in map[string]*FileState) map[string]*FileState {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*FileState, len(in))
	for k, v := range in {
		c := *v
		out[k] = &c
	}
	return out
}

func cloneWidgets(in map[string]*WidgetState) map[string]*WidgetState {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*WidgetState, len(in))
	for k, v := range in {
		c := *v
		if v.State != nil {
			c.State = cloneAnyMap(v.State)
		}
		out[k] = &c
	}
	return out
}

func clonePreviews(in map[string]*PreviewState) map[string]*PreviewState {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*PreviewState, len(in))
	for k, v := range in {
		c := *v
		if v.Payload != nil {
			c.Payload = cloneAnyMap(v.Payload)
		}
		out[k] = &c
	}
	return out
}
