package sessionstore

import (
	"time"
)

type EventType string

const (
	EventUserMessage      EventType = "user_message"
	EventAssistantMessage EventType = "assistant_message"
	// EventSystemMessage is a runtime-emitted system directive (mode switch,
	// background notification, hook inject, reminder, compaction recap, …).
	// It is a durable, sequenced message with Role="system" that the LLM
	// sees in its context at its timeline position, carrying authority over
	// the agent. Distinct from user/assistant so the client can choose not
	// to render it as a chat bubble while the LLM still obeys it. The
	// MessagePayload.Extra map carries {source, position, metadata}.
	EventSystemMessage  EventType = "system_message"
	EventMessageStarted EventType = "message_started"
	EventMessageDone    EventType = "message_done"
	EventToolCall       EventType = "tool_call"
	EventToolResult     EventType = "tool_result"
	// EventToolProgress is a TRANSIENT, client-only signal that one action of a
	// run_parallel batch finished — emitted per child as it completes so the UI
	// updates incrementally instead of waiting for the whole batch. It is NOT
	// handled by the projector (no case below), so it never lands in the agent's
	// message history : the agent still receives the single combined barrier
	// result, unchanged. Generic by construction (emitted from the fan-in, not
	// per tool), so every tool — current and future — is covered.
	EventToolProgress    EventType = "tool_progress"
	EventApprovalRequest EventType = "approval_request"
	EventApprovalGranted EventType = "approval_granted"
	EventApprovalDenied  EventType = "approval_denied"
	EventToolAllowed     EventType = "tool_allowed"
	EventMemoryRemember  EventType = "memory_remember"
	EventMemoryFactAdded EventType = "memory_fact_added"
	EventWorkspaceWrite  EventType = "workspace_write"
	EventWorkspaceEdit   EventType = "workspace_edit"
	EventWorkspaceDelete EventType = "workspace_delete"
	// EventWorkspaceChanges is a TRANSIENT, client-only signal carrying the
	// agent's current pending file changes (git status against the shadow
	// baseline), pushed live to the session room after a coalesced burst of
	// filesystem writes. It is emitted DIRECTLY to the realtime room (never
	// through the durable bus) so it neither lands in the transcript nor
	// consumes a seq — the client holds the authoritative list from
	// GET /workspace/changes and merges these live pushes on top. The projector
	// has no case for it (like tool_progress), so it can never reach durable
	// state even if it ever flowed through the bus.
	EventWorkspaceChanges EventType = "workspace_changes"
	EventAgentSpawn    EventType = "agent_spawn"
	EventAgentProgress EventType = "agent_progress"
	EventAgentResult   EventType = "agent_result"
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
	// EventSessionRenamed updates the session title after creation (the title is
	// otherwise only set on EventSessionStarted). Durable + projected so the rename
	// survives replay/cold-load. Carries the new title in MetaPayload.Title.
	EventSessionRenamed EventType = "session_renamed"
	// EventModelChanged sets the per-session model override (MetaPayload.Model).
	// Durable + projected so the choice survives replay/cold-load. Empty Model
	// clears the override (revert to the Brain default).
	EventModelChanged EventType = "model_changed"
	EventCompactDone  EventType = "compact_done"
	EventQuarantine   EventType = "quarantine"
	EventError        EventType = "error"

	// EventContextCompacting is the START marker of an LLM-context
	// compaction : emitted just BEFORE the (possibly slow, LLM-backed)
	// summarize/truncate work begins, so a client can render a
	// "compacting context…" indicator. It is always paired with the
	// EventContextCompacted END marker that follows, and both flow
	// through the durable AppendDurable path — so their Seqs are
	// strictly ordered (start.Seq < end.Seq) and survive replay. The
	// payload carries the cutoff + dropped count known up-front (from
	// the deterministic safe-split) plus the strategy, so the client can
	// say "summarising N earlier messages" immediately.
	EventContextCompacting EventType = "context_compacting"

	// EventContextCompacted records an LLM-context compaction (distinct
	// from EventCompactDone, which is storage/JSONL compaction). It
	// captures the cutoff seq + injected summary so the COMPACTED VIEW
	// the model sees is reproducible after a resume : the engine drops
	// messages with Seq <= CutoffSeq and prepends Summary. The full
	// history stays on disk untouched — only the LLM's view shrinks.
	// It is the END marker paired with EventContextCompacting.
	EventContextCompacted EventType = "context_compacted"

	// EventContextTokens carries the EXACT occupancy of the model's context
	// window, recomputed in the background by the tokenizer worker (CTX-7) on
	// every context-changing event. The projection sets ContextTokens from it.
	// It is the production source of truth for context_pressure — never an
	// estimate. Emitted off the turn loop ; carried in CtxCompact.NewContextTokens.
	EventContextTokens EventType = "context_tokens"

	// EventContextSummaryPrepared carries a high-fidelity LLM summary of the
	// session's aged region, produced PROACTIVELY off the turn loop by the
	// summary maintainer (CTX-8). It is a CANDIDATE the compaction gate applies
	// INSTANTLY — it does NOT itself change the model's view, so the loop never
	// blocks on a summary LLM call. CoversSeq is the Seq the summary accounts up
	// to; the gate applies it as an EventContextCompacted at that cutoff.
	// Coalesced (non-durable); the prepared candidate rides the snapshot.
	EventContextSummaryPrepared EventType = "context_summary_prepared"

	// Turn lifecycle events (RT-1). Emitted by the runtime turn package
	// to record each phase transition durably. Replaying the events
	// rebuilds CurrentTurnID + CurrentTurnPhase so crash recovery can
	// surface "this turn was mid-flight when the daemon died" cleanly.
	EventTurnStarted      EventType = "turn_started"
	EventTurnPhaseChanged EventType = "turn_phase_changed"
	EventTurnEnded        EventType = "turn_ended"

	// EventTurnRetry is emitted when the engine AUTO-RETRIES a transient LLM
	// failure (network drop, rate limit, provider 5xx) — and ONLY before any
	// token of the failed attempt streamed, so a restart can never duplicate
	// output. It is DURABLE on purpose : it rides the same per-session seq log
	// as everything else, so a client that was disconnected while the daemon
	// was retrying REPLAYS it on reconnect and learns a retry was in flight.
	// That is what makes the two cut-points (client↔daemon, daemon↔provider)
	// synchronise through one ordered stream. The client maps it to opencode's
	// native "retry" status (attempt #, countdown). RetryPayload carries the
	// classified cause + the backoff before the next attempt.
	EventTurnRetry EventType = "turn_retry"

	// EventAssistantDelta (R-4) is the per-token streaming event.
	// Emitted by Engine.RunStreaming for every ChatChunk the LLM
	// produces, so subscribers (Socket.IO, CLI streaming view) can
	// render tokens as they arrive — no need to wait for the full
	// assistant message.
	//
	// Carried on Message.Parts as a single Text part with the
	// delta content. CorrelationID is the turn ID ; Seq orders
	// deltas globally within the session. The final
	// EventAssistantMessage still fires at the end of the round
	// with the consolidated content, so subscribers that don't
	// care about streaming can ignore deltas.
	EventAssistantDelta EventType = "assistant_delta"

	// EventAssistantReasoningDelta streams the agent's thinking-mode trace
	// (reasoning_content) live during generation, like EventAssistantDelta does
	// for the visible text. TRANSIENT : the projector ignores it (no case), so
	// it never lands in durable state — the consolidated reasoning is persisted
	// on the final EventAssistantMessage (MessagePayload.Reasoning). The chunk
	// rides MessagePayload.Reasoning so the client reads payload.reasoning.
	EventAssistantReasoningDelta EventType = "assistant_reasoning_delta"

	// EventSecurityDecision (SG-6) is the documented audit row that the
	// security gates emit on every NON-BYPASS evaluation. Per the doc
	// (docs-site/docs/language/11-security.md "Every gate decision must
	// emit a structured audit row"), the payload carries app/agent/
	// session routing, the (module, action) being decided, the gate
	// that produced the decision, the decision itself, and the params
	// (with sensitive values redacted). System modules and meta-tools
	// bypass the gates entirely and produce NO audit row — this keeps
	// the durable log free of infrastructure noise.
	EventSecurityDecision EventType = "security_decision"

	// EventBackgroundTask records a background_run task lifecycle
	// transition (launched / completed / errored / cancelled). It carries
	// the real session/app/user routing so the Socket.IO bridge pushes it
	// to the session room in real time, and so the client can render and
	// cancel running tasks live. The State field is the discriminator ;
	// the durable trail also lets a reconnecting / cold-loading client
	// reconstruct the task list from the event stream.
	EventBackgroundTask EventType = "background_task"
)

type Event struct {
	Seq           uint64    `json:"seq"`
	Type          EventType `json:"type"`
	TsUnixNano    int64     `json:"ts"`
	SessionID     string    `json:"session_id"`
	AppID         string    `json:"app_id,omitempty"`
	UserID        string    `json:"user_id,omitempty"`
	CorrelationID string    `json:"correlation_id,omitempty"`

	// LiveOutputTokens is the running ~estimate of tokens generated so far in
	// the current assistant message — set ONLY on EventAssistantDelta during
	// streaming so a client renders a smooth, incrementing token counter at the
	// rhythm tokens arrive (CTX-7.5). It is a cheap chars/4 estimate, not the
	// exact count : the round-end EventTokenUsage anchor carries the provider's
	// exact total, which the client snaps to. Omitted (0) on every other event.
	LiveOutputTokens int `json:"live_output_tokens,omitempty"`

	// CtxTokens carries the EXACT context breakdown (system/tools/messages)
	// from the background Context Service (CTX-7). Set only on
	// EventContextTokens.
	CtxTokens *ContextTokensPayload `json:"ctx_tokens,omitempty"`

	Message    *MessagePayload          `json:"message,omitempty"`
	Tool       *ToolPayload             `json:"tool,omitempty"`
	Approval   *ApprovalPayload         `json:"approval,omitempty"`
	Memory     *MemoryPayload           `json:"memory,omitempty"`
	Workspace  *WorkspacePayload        `json:"workspace,omitempty"`
	Agent      *AgentPayload            `json:"agent,omitempty"`
	Widget     *WidgetPayload           `json:"widget,omitempty"`
	Preview    *PreviewPayload          `json:"preview,omitempty"`
	Todo       *TodoPayload             `json:"todo,omitempty"`
	Cost       *CostPayload             `json:"cost,omitempty"`
	Compact    *CompactPayload          `json:"compact,omitempty"`
	CtxCompact *ContextCompactPayload   `json:"ctx_compact,omitempty"`
	CtxSummary *ContextSummaryPayload   `json:"ctx_summary,omitempty"`
	Meta       *MetaPayload             `json:"meta,omitempty"`
	Error      *ErrorPayload            `json:"error,omitempty"`
	Turn       *TurnPayload             `json:"turn,omitempty"`
	Retry      *RetryPayload            `json:"retry,omitempty"`
	Security   *SecurityDecisionPayload `json:"security,omitempty"`
	Background *BackgroundTaskPayload   `json:"background,omitempty"`
	// WorkspaceChanges carries the live pending-changes list for the workspace
	// preview push. Set only on EventWorkspaceChanges (transient).
	WorkspaceChanges *WorkspaceChangesPayload `json:"workspace_changes,omitempty"`
	Allowed          *AllowedToolPayload      `json:"allowed,omitempty"`
}

type AllowedToolPayload struct {
	Signature string `json:"signature"`
}

// RetryPayload describes one auto-retry of a transient LLM failure, carried on
// EventTurnRetry. Attempt is the 1-based number of the attempt ABOUT TO run
// (the original try is 1, so the first retry reports 2) and Max is the total
// attempt budget — together they render "attempt #2/4". RetryInMs is the
// backoff the engine waits before that attempt, so the client can show a live
// countdown (next = now + retry_in_ms). Message/Code/Category come from the
// errclass classification of the cause.
type RetryPayload struct {
	Attempt   int    `json:"attempt"`
	Max       int    `json:"max"`
	Message   string `json:"message"`
	Code      string `json:"code,omitempty"`
	Category  string `json:"category,omitempty"`
	RetryInMs int    `json:"retry_in_ms"`
}

// WorkspaceChangesPayload is the live pending-changes snapshot pushed to the
// client after the agent edits files — the same shape the GET /workspace/changes
// REST endpoint returns, so the client renders both identically.
type WorkspaceChangesPayload struct {
	Files []WorkspaceChangedFile `json:"files"`
	Count int                    `json:"count"`
}

// WorkspaceChangedFile is one pending change : a path relative to the workdir
// and its status ("added" | "modified" | "deleted"). Diffs are fetched lazily
// by the client via GET /workspace/diff?path=… — they are not inlined here.
type WorkspaceChangedFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// BackgroundTaskPayload describes one background_run task lifecycle
// transition. Emitted by the background.Manager on launch / completion /
// error / cancellation.
//
// Fields :
//   - TaskID       : the uuid returned to the agent at launch
//   - Tool         : the launched tool FQN (e.g. "database.sql")
//   - State        : "running" | "completed" | "errored" | "cancelled"
//   - Error        : failure reason when State == "errored"
//   - ElapsedMs    : wall time from launch to terminal state (0 while running)
//   - StartedAtUnix: launch time (unix seconds)
//
// The full result is intentionally NOT carried here — per
// docs-site/language/04c-primitives.md the agent re-fetches it via
// background_run(task_id=...). The client renders status from the State.
type BackgroundTaskPayload struct {
	TaskID        string `json:"task_id"`
	Tool          string `json:"tool"`
	Label         string `json:"label,omitempty"`
	State         string `json:"state"`
	Error         string `json:"error,omitempty"`
	ElapsedMs     int64  `json:"elapsed_ms,omitempty"`
	StartedAtUnix int64  `json:"started_at_unix,omitempty"`
}

// SecurityDecisionPayload is the audit row emitted by the security
// gates on every non-bypass evaluation (SG-6). Documented in
// docs-site/docs/language/11-security.md.
//
// Fields :
//   - AppID/AgentID/SessionID/UserID : routing (also on Event top-level
//     fields ; duplicated here for self-contained audit consumers)
//   - Module/Action            : the (FQN) being decided
//   - RiskLevel                : intrinsic action risk (low|medium|high)
//   - ParamsRedacted           : tool_params with sensitive keys
//     replaced by "[REDACTED]"
//   - Decision                 : "allow"|"deny"|"needs_approval"
//   - Gate                     : "gate0_inactive"|"gate1a_module"|... or
//     "system_module_bypass"/"meta_tool_bypass"
//   - Reason                   : human-readable explanation surfaced to
//     the audit log and UI
//   - Caller                   : "llm"|"hook"|"setup"|"channel"|"internal"
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

// MessagePayload is one message in the session log. The persisted shape
// is multi-part to natively support text + images + tool calls in a
// single message, matching what LLM providers (OpenAI/Anthropic/Gemini)
// expect on the wire.
//
// Parts is the SOURCE OF TRUTH. Content / Attachments / ToolCallIDs are
// kept for back-compat with readers that don't know about Parts yet —
// projection derives them from Parts. New code MUST read Parts.
type MessagePayload struct {
	Role  string        `json:"role"`
	Parts []MessagePart `json:"parts,omitempty"`

	// Reasoning is the assistant's thinking-mode trace (DeepSeek/o-series),
	// kept separate from the visible content parts. Persisted so it round-trips
	// back to providers that require the prior reasoning_content on assistant
	// messages — dropping it makes DeepSeek thinking mode reject the turn.
	Reasoning string `json:"reasoning,omitempty"`
	ReasoningStartedAt int64 `json:"reasoning_started_at,omitempty"`
	ReasoningEndedAt   int64 `json:"reasoning_ended_at,omitempty"`

	// ClientMessageID is the caller's idempotency key for a user message,
	// echoed verbatim so a client reconciles its optimistic bubble with the
	// durable event (and the seq it carries) deterministically.
	ClientMessageID string `json:"client_message_id,omitempty"`

	// Legacy fields. Still populated on read for back-compat ; writers
	// that fill Parts SHOULD leave these zero — projection synthesizes.
	Content     string         `json:"content,omitempty"`
	ToolCallIDs []string       `json:"tool_call_ids,omitempty"`
	Attachments []BlobRef      `json:"attachments,omitempty"`
	Extra       map[string]any `json:"extra,omitempty"`
}

// MessagePart is one chunk of a multi-part message. The Type field is
// the discriminator ; exactly one of Text/Blob/ToolCall/ToolResult is
// filled depending on Type :
//
//   - "text"        → Text
//   - "image"|"audio"|"video"|"file" → Blob
//   - "tool_call"   → ToolCall   (assistant invokes a tool)
//   - "tool_result" → ToolResult (response to a tool_call)
//
// Unknown types are preserved through persistence verbatim and skipped
// by the LLM adapter — forward-compat for future formats.
type MessagePart struct {
	Type string `json:"type"`

	Text       string          `json:"text,omitempty"`
	Blob       *BlobRef        `json:"blob,omitempty"`
	ToolCall   *ToolCallSpec   `json:"tool_call,omitempty"`
	ToolResult *ToolResultSpec `json:"tool_result,omitempty"`
}

// Part type discriminators. Use these constants instead of string
// literals to keep producers and consumers in sync.
const (
	PartTypeText       = "text"
	PartTypeImage      = "image"
	PartTypeAudio      = "audio"
	PartTypeVideo      = "video"
	PartTypeFile       = "file"
	PartTypeToolCall   = "tool_call"
	PartTypeToolResult = "tool_result"
)

// ToolCallSpec is the assistant's structured "run tool X with these
// args" emission. Persisted as a part inside an assistant Message.
// The runtime dispatches the call to the appropriate module ; the
// result lands as a "tool" role Message containing a ToolResultSpec.
type ToolCallSpec struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// ToolResultSpec is the result of running a tool call. Multi-part so
// a tool can return mixed content (e.g. a screenshot tool returning
// text + image). Error is non-empty when the tool failed ; Parts may
// still be present to carry partial output.
type ToolResultSpec struct {
	ToolCallID string        `json:"tool_call_id"`
	Parts      []MessagePart `json:"parts,omitempty"`
	Error      string        `json:"error,omitempty"`
}

type ToolPayload struct {
	CallID    string         `json:"call_id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
	Status    string         `json:"status,omitempty"`
	// LiveTokens is the running ~estimate of the tool call's argument tokens
	// while it streams (status="streaming"). Lets the client grow a counter so
	// the user sees a long write progressing instead of a frozen UI.
	LiveTokens int `json:"live_tokens,omitempty"`
	// Output is the legacy text-only result. New code should fill Parts
	// instead — it supports multi-format (text + image for screenshots,
	// audio for TTS tools, etc.).
	Output     any           `json:"output,omitempty"`
	Parts      []MessagePart `json:"parts,omitempty"`
	Error      string        `json:"error,omitempty"`
	DurationMs int64         `json:"duration_ms,omitempty"`
	// Client-only display fields (top-level on the wire, matching the legacy
	// daemon so existing clients render diffs unchanged). The LLM never sees
	// these — the adapter projects only Parts/Output into the model context.
	Diff            string         `json:"diff,omitempty"`             // short human summary
	UnifiedDiff     string         `json:"unified_diff,omitempty"`     // parseable unified diff
	PreviousContent string         `json:"previous_content,omitempty"` // before (capped)
	NewContent      string         `json:"new_content,omitempty"`      // after (capped)
	Metadata        map[string]any `json:"metadata,omitempty"`         // side-channel (bytes_written, additions, …)
}

// ApprovalPayload carries the documented approval request shape from
// docs-site/docs/tutorial/security-01-approval.md. The payload visible
// to clients via GET /api/apps/{id}/approvals includes :
//
//	{request_id, agent_id, user_id, app_id, session_id, tool_name,
//	 tool_params, risk_level, reason}
//
// In this struct :
//   - ID maps to the documented "request_id" (canonical name in the
//     existing Go REST contract since T5).
//   - Kind is "tool_call" for SG-5 approvals ; "write_file" or other
//     values pre-existed in earlier sprints.
//   - Payload is the legacy free-form payload for custom approval
//     kinds (non-tool_call).
//   - AgentID/ToolName/ToolParams/RiskLevel are the SG-5 additions
//     that mirror the doc's JSON payload bit-for-bit.
//   - Status moves through "pending" → "granted"/"denied"/"auto_denied"
//     as resolution proceeds.
//   - Reason is the human-readable text (from CapabilityGrant.Reason
//     on request, from the resolver's message on grant/deny).
//
// The event's top-level fields (SessionID, AppID, UserID) carry the
// remaining doc fields ; the payload doesn't duplicate them.
type ApprovalPayload struct {
	ID      string         `json:"id"`
	Kind    string         `json:"kind"`
	Payload map[string]any `json:"payload,omitempty"`
	Status  string         `json:"status,omitempty"`
	Reason  string         `json:"reason,omitempty"`

	// SG-5 additions for tool-call approvals. All optional ; legacy
	// approval kinds leave them zero.
	AgentID    string         `json:"agent_id,omitempty"`
	ToolName   string         `json:"tool_name,omitempty"`
	ToolParams map[string]any `json:"tool_params,omitempty"`
	RiskLevel  string         `json:"risk_level,omitempty"`
	// CallID binds the approval to the in-flight tool_call chip so a client can
	// flip that exact chip to an "awaiting approval" state (and back to running
	// once granted) instead of showing a misleading spinner.
	CallID string `json:"call_id,omitempty"`
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

	// Telemetry — present on agent_progress (live) and agent_result (terminal).
	// Durable so a reconnecting client reconstructs the full agent tree from replay
	// without hitting the live registry (which is gone after a daemon restart).
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

type TodoPayload struct {
	ID     string `json:"id"`
	Text   string `json:"text,omitempty"`
	Status string `json:"status,omitempty"`
}

type CostPayload struct {
	TokensIn  int64 `json:"tokens_in,omitempty"`
	TokensOut int64 `json:"tokens_out,omitempty"`
	// ReasoningTokens is the provider-exact subset of TokensOut spent on hidden
	// reasoning (already included in TokensOut — a breakdown, never additive).
	ReasoningTokens int64   `json:"reasoning_tokens,omitempty"`
	UsdTotal        float64 `json:"usd_total,omitempty"`
	// Prompt-cache accounting : how many of TokensIn were served from the
	// provider's cached prefix (read) vs written to the cache this turn. A high
	// CacheReadTokens on later turns is the proof the stable-prefix cache works —
	// those tokens skip prefill, which is the TTFT win.
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
}

type CompactPayload struct {
	CutoffSeq      uint64 `json:"cutoff_seq"`
	SnapshotSHA256 string `json:"snapshot_sha256"`
	Binary         bool   `json:"binary,omitempty"`
	EventsBefore   int    `json:"events_before"`
	BytesBefore    int64  `json:"bytes_before"`
	DurationMs     int64  `json:"duration_ms"`
}

// ContextCompactPayload records an LLM-context compaction pass : every
// message with Seq <= CutoffSeq is hidden from the model's prompt and
// replaced by the single Summary system message. KeepRecent / Strategy
// / MessagesDropped are kept for audit + observability.
type ContextCompactPayload struct {
	CutoffSeq       uint64 `json:"cutoff_seq"`
	Summary         string `json:"summary,omitempty"`
	KeepRecent      int    `json:"keep_recent,omitempty"`
	Strategy        string `json:"strategy,omitempty"`
	MessagesDropped int    `json:"messages_dropped,omitempty"`
	TokensBefore    int    `json:"tokens_before,omitempty"`
	// TokensFreed is the estimated tokens removed by this compaction
	// (before − the compacted view). Informational, for the client's "N tokens
	// freed" summary ; the EXACT occupancy still comes from the tokenizer.
	TokensFreed int `json:"tokens_freed,omitempty"`
	// NewContextTokens is the FULL occupancy of the compacted view on the same
	// scale as the window : the fixed overhead (system prompt + tool schemas,
	// from the EXACT recount) + the kept conversation (summary + recent). The
	// compactor sets it synchronously so a client's post-compaction "ctx
	// used/window" is the real size sent to the model, not a messages-only figure.
	// 0 = not set (the gauge keeps its prior value / the client falls back).
	NewContextTokens int `json:"new_context_tokens,omitempty"`
}

// ContextTokensPayload is the EXACT occupancy breakdown of the model's context
// window (CTX-7) : the three budget buckets and their sum. Total is the gauge
// (context_pressure numerator) ; the split tells a client / a turn variable
// where the budget goes (system prompt vs tool schemas vs conversation).
type ContextTokensPayload struct {
	Total    int `json:"total"`
	System   int `json:"system,omitempty"`
	Tools    int `json:"tools,omitempty"`
	Messages int `json:"messages,omitempty"`
	// Window is the model's raw context window (the denominator clients show as
	// "used / window") — the configured context.max_tokens, or the model's
	// documented window when that is 0. Limit is the usable INPUT budget
	// (Window − output_reserved) : the pressure denominator compaction compares
	// against. Both ride every context_tokens event so a client gauge matches
	// the daemon's own numbers exactly.
	Window int `json:"window,omitempty"`
	Limit  int `json:"limit,omitempty"`
}

// ContextSummaryPayload is a PREPARED high-fidelity LLM summary of the aged
// region (CTX-8), produced off the turn loop by the summary maintainer. It is a
// candidate the compaction gate applies instantly (emitting an
// EventContextCompacted at CoversSeq with this Summary) so the loop never waits
// on a summary LLM call. CoversSeq is the Seq of the last message the summary
// accounts for; the kept view is messages with Seq > CoversSeq.
type ContextSummaryPayload struct {
	Summary     string `json:"summary"`
	CoversSeq   uint64 `json:"covers_seq"`
	InputTokens int    `json:"input_tokens,omitempty"`
	Model       string `json:"model,omitempty"`
}

type MetaPayload struct {
	Title       string `json:"title,omitempty"`
	Workspace   string `json:"workspace,omitempty"`
	Workdir     string `json:"workdir,omitempty"`
	Interrupted bool   `json:"interrupted,omitempty"`
	// Model + AgentID carry a per-session, per-agent model override on
	// EventModelChanged : set AgentID's model to Model (empty Model clears it →
	// revert that agent to its Brain default). MaxContextTokens is the gateway's
	// documented window for this model, persisted at switch time so the daemon
	// never needs a user token to resolve the context window — survives restart.
	Model            string `json:"model,omitempty"`
	AgentID          string `json:"agent_id,omitempty"`
	MaxContextTokens int    `json:"max_ctx_tokens,omitempty"`
	// EntryAgent pins which agent handles this session (overrides the app's YAML
	// entry agent) and ContextExtra is extra system-prompt text for the session.
	// Both are set at creation by non-human launchers (e.g. a background channel
	// trigger) and are empty for ordinary sessions.
	EntryAgent   string `json:"entry_agent,omitempty"`
	ContextExtra string `json:"context,omitempty"`
	// Actor is the real caller identity when the session was created on behalf of
	// another user (a trusted service impersonating an end-user via X-Act-As-User).
	// UserID is the OWNER (the end-user) ; Actor is WHO launched it. Empty for
	// ordinary sessions where owner == caller.
	Actor string `json:"actor,omitempty"`
}

type ErrorPayload struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
	// DaemonError contract consumed by web/flutter clients (parseDaemonError):
	// a turn-failure `error` event carries these so the client renders the
	// banner and decides whether to offer Retry. Error falls back to Message
	// client-side; Retry is a pointer so an explicit false survives omitempty
	// (a missing retry defaults to true on the client).
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
}

// TurnPayload carries lifecycle data for the three turn events. TurnID
// is a UUID minted by the runtime once per turn ; subsequent phase
// changes and the end event reuse the same ID so consumers can
// correlate. Phase is filled on EventTurnPhaseChanged ; Status is
// filled on EventTurnEnded ("done"|"errored"|"interrupted").
// AgentID identifies which agent of the app is running the turn
// (default = first agent in V1, picked per session in V2).
type TurnPayload struct {
	TurnID  string `json:"turn_id"`
	AgentID string `json:"agent_id,omitempty"`
	Phase   string `json:"phase,omitempty"`
	Status  string `json:"status,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

func (e Event) Time() time.Time { return time.Unix(0, e.TsUnixNano).UTC() }
