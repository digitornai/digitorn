package sessionstore

import (
	"sync"
	"time"
)

// SessionState is the projected, live view of one session. Every field
// is a CACHE — the source of truth lives on disk as the JSONL event
// log. The invariant we maintain :
//
//   - Messages and the other projected slices/maps are mutated EXCLUSIVELY
//     by Apply / applyLocked (the projection function). Nothing outside
//     this package touches them.
//   - Apply is called only from durable write paths : AppendDurable
//     after the event lands on disk, and Load during cold rehydrate.
//   - Reads are concurrent-safe under the RWMutex ; writes are serial.
//
// Net effect : Messages always reflects exactly what's persisted, and a
// turn handler can call State() any number of times without paying a
// re-projection cost. Cold rehydrate of 1000 events lands ~ 8ms on
// commodity SSD (see cold_load_regression_test.go).
type SessionState struct {
	mu sync.RWMutex

	SessionID     string
	AppID         string
	UserID        string
	StartedAtNano int64
	EndedAtNano   int64
	// LastEventTsNano is the ts of the most recently applied event — the
	// authoritative "updated at", flushed to meta.json so the list endpoint can
	// order sessions without reading their events file.
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

	// SeenClientMsgIDs maps each user message's client_message_id → its seq, so a
	// re-delivery with the same id (a background-job retry, a client resend) is an
	// idempotent no-op rather than a duplicate message + a spurious extra turn.
	SeenClientMsgIDs map[string]uint64

	// Tools is the toolset exposed to the session (name + schema), surfaced in
	// the history response so a client can render what the agent could call.
	Tools []ToolSpec

	// ContextCompaction is the LATEST LLM-context compaction marker (nil
	// until the first compaction). The engine uses it to build the
	// model's prompt view : messages with Seq <= CutoffSeq are hidden
	// and replaced by Summary. Projected from EventContextCompacted ;
	// persisted in the snapshot so it survives a daemon restart.
	ContextCompaction *ContextCompactionState

	// CompactionInflight is true between an EventContextCompacting (start)
	// and its paired EventContextCompacted (end) — so a client that loads
	// the state snapshot mid-compaction (rather than replaying events)
	// still knows to show the "compacting…" indicator.
	CompactionInflight bool

	// ContextTokens is the OCCUPANCY gauge : how full the model's context
	// window is right now, in tokens. Unlike TokensIn/TokensOut (which are
	// CUMULATIVE cost counters summed over every turn), this is a GAUGE —
	// last-value-wins, set to the last LLM round's (prompt+completion)
	// reported by the provider (the authoritative count of exactly what
	// entered the window). It is what context_pressure divides by MaxTokens.
	// 0 means "no provider anchor yet" → callers fall back to a local
	// estimate. Distinct field on purpose : cost accumulates, occupancy does
	// not — conflating them is the bug CTX-7 fixes.
	ContextTokens int

	// Context occupancy breakdown (CTX-7) : the three budget buckets that sum
	// to ContextTokens. Set by EventContextTokens from the background recount.
	ContextSystemTokens  int
	ContextToolsTokens   int
	ContextMessageTokens int

	// ContextProviderTokens is the provider's EXACT context count (prompt +
	// completion) from the last turn — the ground truth for ANY provider. Set by
	// EventTokenUsage, reset to 0 by EventContextCompacted (the provider hasn't
	// seen the compacted context yet). The background recount uses it to learn a
	// per-session tokenizer→provider calibration ratio so the displayed gauge
	// tracks the real provider count, not the raw tiktoken estimate.
	ContextProviderTokens int

	// PreparedSummary is the LATEST high-fidelity LLM summary of the aged region
	// (CTX-8), produced PROACTIVELY off the turn loop by the summary maintainer
	// and set by EventContextSummaryPrepared. It is a CANDIDATE only — it does
	// NOT change the model's view; the compaction gate applies it instantly when
	// pressure trips, so the loop never blocks on a summary LLM call. Cleared
	// once consumed (an EventContextCompacted whose cutoff reaches CoversSeq).
	PreparedSummary *PreparedSummaryState

	TokensIn  int64
	TokensOut int64
	UsdTotal  float64

	Title        string
	Workspace    string
	Workdir      string
	EntryAgent   string
	ContextExtra string
	// ModelOverrides maps an agent's logical id → the per-session model the user
	// pinned for it (PUT /sessions/{id}/model). It overrides that agent's Brain
	// default for this session's turns. Held on the PARENT session ; sub-agents
	// run in ephemeral child sessions but read their override from here, keyed by
	// their logical id (so the choice applies to every future sub-turn). nil/empty
	// = every agent uses its Brain default.
	ModelOverrides map[string]string
	// EntryModelWindow is the gateway's documented max_context_tokens for the
	// entry agent's selected model, persisted from EventModelChanged so the
	// background recount resolves the real window without a gateway call.
	EntryModelWindow int
	TurnCount        int
	Interrupted      bool
	// LastUserMessage is the verbatim text of the most recent user message.
	// Updated by every EventUserMessage projection — survives compaction so
	// working memory always knows what the agent must currently answer.
	LastUserMessage string

	// CurrentTurnID identifies the in-flight turn, or "" when no turn is
	// running. CurrentTurnPhase tracks its state across phase changes ;
	// CurrentTurnStartedAtNano lets recovery detect stale turns and the
	// observability layer compute per-turn durations without scanning
	// events. All three are projected from EventTurnStarted /
	// EventTurnPhaseChanged / EventTurnEnded.
	CurrentTurnID            string
	CurrentTurnPhase         string
	CurrentTurnStartedAtNano int64

	// ActiveMode is the composer mode (runtime.modes) currently bound to
	// this session — the mode the last mode-switch directive announced.
	// Projected from EventSystemMessage with source="mode_switch" so it's
	// reconstructed verbatim on cold-load. Empty = no mode active. Drives
	// the per-turn stickiness (an omitted mode reuses it) and the
	// "did the mode change?" check that gates re-emitting the directive.
	ActiveMode string

	Closed bool

	EventCount uint64
	BytesEst   int64
	Partial    bool
}

// ToolSpec is a tool declaration exposed to the session : its name, a human
// description, and its JSON-schema parameters. Surfaced in the history view.
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// Message is one entry in the projected session message log. Parts is
// the source of truth ; Content / ToolCallIDs / Attachments are derived
// during projection for back-compat readers (TUI, web, legacy SDK).
//
// Tools always look at Parts. Display layers can keep using Content as
// long as they accept that it's only the text portion of the message.
type Message struct {
	Seq         uint64        `json:"seq"`
	Role        string        `json:"role"`
	Parts       []MessagePart `json:"parts,omitempty"`
	Content     string        `json:"content"`
	Reasoning   string        `json:"reasoning,omitempty"`
	ReasoningStartedAt int64         `json:"reasoning_started_at,omitempty"`
	ReasoningEndedAt   int64         `json:"reasoning_ended_at,omitempty"`
	TsUnixNano  int64         `json:"ts"`
	ToolCallIDs []string      `json:"tool_call_ids,omitempty"`
	Attachments []BlobRef     `json:"attachments,omitempty"`
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

	// SG-5 : projected approval payload fields documented in
	// docs-site/docs/tutorial/security-01-approval.md. GET
	// /api/apps/{id}/approvals must surface these so the UI can show
	// what the agent is asking permission for.
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
	Status         string `json:"status"` // running | completed | errored | cancelled | interrupted
	ResultSummary  string `json:"result_summary,omitempty"`
	Depth          int    `json:"depth,omitempty"`
	SpawnedAt      int64  `json:"spawned_at"`
	CompletedAt    int64  `json:"completed_at,omitempty"`
	UpdatedAt      int64  `json:"updated_at,omitempty"`

	// Live telemetry — updated on every agent_progress event so a reconnecting
	// or cold-loaded client sees latest known state without hitting the registry.
	ToolCalls   int64  `json:"tool_calls,omitempty"`
	LLMCalls    int64  `json:"llm_calls,omitempty"`
	TokensIn    int64  `json:"tokens_in,omitempty"`
	TokensOut   int64  `json:"tokens_out,omitempty"`
	Children    int64  `json:"children,omitempty"`
	DurationMs  int64  `json:"duration_ms,omitempty"`
	CurrentTool string `json:"current_tool,omitempty"`
}

// BackgroundTaskState is the DURABLE, projected view of one background_run task,
// rebuilt from EventBackgroundTask. Unlike the in-memory BackgroundManager
// registry (lost on restart), this survives a daemon restart and cold-load, so
// a reconnecting client reconstructs the task list from the event log. A task
// left "running" at cold-load is reconciled to "interrupted" (the goroutine
// died with the daemon).
type BackgroundTaskState struct {
	TaskID        string `json:"task_id"`
	Tool          string `json:"tool,omitempty"`
	Label         string `json:"label,omitempty"`
	State         string `json:"state"` // running | completed | errored | cancelled | interrupted
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

// ContextCompactionState is the latest LLM-context compaction marker.
// CutoffSeq : messages with Seq <= CutoffSeq are hidden from the model.
// Summary   : the system message injected in their place.
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

// PreparedSummaryState is a background-prepared LLM summary candidate (CTX-8).
// CoversSeq : the Seq up to which Summary accounts. The compaction gate applies
// it as an EventContextCompacted at this cutoff — it never blocks the turn.
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

// RLock acquires the read lock on the session state. Callers MUST pair it
// with a deferred RUnlock(). Use for short, allocation-free reads only.
func (s *SessionState) RLock()   { s.mu.RLock() }
func (s *SessionState) RUnlock() { s.mu.RUnlock() }

// SeenClientMessage reports whether a user message with this client_message_id has
// already been recorded, and its seq. Empty id → never seen. The append path uses it
// to make a re-delivery (retry / resend) idempotent instead of a duplicate.
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
			EntryModelWindow:         s.EntryModelWindow,
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
