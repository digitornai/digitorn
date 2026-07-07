package sessionstore

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type HistoryResponse struct {
	Messages       []HistoryMessage `json:"messages"`
	Events         []HistoryEvent   `json:"events"`
	EventCount     int              `json:"event_count"`
	EventsTotal    int              `json:"events_total"`
	EventsNextSeq  uint64           `json:"events_next_seq"`
	EventsHasMore  bool             `json:"events_has_more"`
	InstanceID     string           `json:"instance_id,omitempty"`
	TurnActive     bool             `json:"turn_active"`
	PendingQueue   []any            `json:"pending_queue"`
	Title          string           `json:"title,omitempty"`
	Workspace      string           `json:"workspace,omitempty"`
	Workdir        string           `json:"workdir,omitempty"`
	LastSeq        uint64           `json:"last_seq"`
	SnapshotCutoff uint64           `json:"snapshot_cutoff,omitempty"`
	Tools          []map[string]any `json:"tools,omitempty"`
}

type HistoryMessage struct {
	Role        string    `json:"role"`
	Content     string    `json:"content"`
	Reasoning   string    `json:"reasoning,omitempty"`
	ReasoningStartedAt int64     `json:"reasoning_started_at,omitempty"`
	ReasoningEndedAt   int64     `json:"reasoning_ended_at,omitempty"`
	Seq         uint64        `json:"seq"`
	StepID      string        `json:"step_id,omitempty"`
	Ts          string        `json:"ts"`
	ToolCalls   []any         `json:"tool_calls,omitempty"`
	Attachments []BlobRef     `json:"attachments,omitempty"`
	// Parts carries the full multi-part shape (text/image/video/…) so the
	// denormalized history fallback can render generated media the same way
	// the live + event-replay paths do.
	Parts []MessagePart `json:"parts,omitempty"`
}

type HistoryEvent struct {
	ID            string `json:"id"`
	Ts            string `json:"ts"`
	Seq           uint64 `json:"seq"`
	Kind          string `json:"kind"`
	Type          string `json:"type"`
	Payload       any    `json:"payload,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

type EventsResponse struct {
	Events []HistoryEvent `json:"events"`
	Count  int            `json:"count"`
	Total  int            `json:"total"`
}

// SocketEnvelope is the wire format for every Socket.IO event emitted to
// clients on the `/events` namespace. The shape MUST stay byte-for-byte
// compatible with the legacy Python daemon — dozens of web clients depend
// on these field names.
type SocketEnvelope struct {
	EventID             string   `json:"event_id,omitempty"`
	Type                string   `json:"type"`
	Kind                string   `json:"kind"`
	Seq                 uint64   `json:"seq"`
	AppID               string   `json:"app_id,omitempty"`
	SessionID           string   `json:"session_id,omitempty"`
	Payload             any      `json:"payload,omitempty"`
	Ts                  string   `json:"ts"`
	Control             bool     `json:"control"`
	Capabilities        []string `json:"capabilities,omitempty"`
	UserID              string   `json:"user_id,omitempty"`
	InstanceID          string   `json:"instance_id"`
	DroppedPreBootstrap bool     `json:"_dropped_pre_bootstrap,omitempty"`

	// AgentRunID / RootSessionID are set ONLY on the copy of a sub-agent event
	// that the bridge fans out to its root session room. They let a client
	// watching the top-level session attribute the event to the right sub-agent
	// chip without parsing the sub-session id. Absent (omitempty) on every
	// normal event — backward compatible with the legacy wire shape.
	AgentRunID    string `json:"agent_run_id,omitempty"`
	RootSessionID string `json:"root_session_id,omitempty"`
	StepID        string `json:"step_id,omitempty"`

	// CorrelationID ties a transient event to a parent it belongs to : a
	// run_parallel child's tool_progress carries the parent run_parallel
	// call_id here, so the client can update the right chip without parsing
	// the synthetic child id. Absent (omitempty) on plain events.
	CorrelationID string `json:"correlation_id,omitempty"`

	// LiveOutputTokens mirrors Event.LiveOutputTokens — the running token
	// estimate carried on a streaming delta so the client increments a live
	// counter (CTX-7.5). Absent (0) on non-delta events.
	LiveOutputTokens int `json:"live_output_tokens,omitempty"`
}

type ViewOptions struct {
	IncludePayload bool
	StartSeq       uint64
	MaxEvents      int
	InstanceID     string
}

// EnvelopeBuilder produces SocketEnvelopes with daemon-scoped fields
// (instance_id, capabilities) pre-filled. One builder per Daemon instance.
type EnvelopeBuilder struct {
	InstanceID   string
	Capabilities []string
}

func NewEnvelopeBuilder(instanceID string, capabilities []string) *EnvelopeBuilder {
	if instanceID == "" {
		instanceID = generateInstanceID()
	}
	return &EnvelopeBuilder{InstanceID: instanceID, Capabilities: capabilities}
}

func (b *EnvelopeBuilder) Build(ev *Event) SocketEnvelope {
	if ev == nil {
		return SocketEnvelope{}
	}
	return SocketEnvelope{
		EventID:          fmt.Sprintf("%s:%d", ev.SessionID, ev.Seq),
		Type:             string(ev.Type),
		Kind:             envelopeKindFor(ev.Type),
		Seq:              ev.Seq,
		AppID:            ev.AppID,
		SessionID:        ev.SessionID,
		Payload:          eventPayload(ev),
		Ts:               unixNanoToISO(ev.TsUnixNano),
		Control:          isControlEvent(ev.Type),
		Capabilities:     b.Capabilities,
		UserID:           ev.UserID,
		InstanceID:       b.InstanceID,
		CorrelationID:    ev.CorrelationID,
		StepID:           ev.StepID,
		LiveOutputTokens: ev.LiveOutputTokens,
	}
}

// BuildHistory renders a session into the legacy daemon's GET /history shape.
// The shape is fixed by the client contract — do NOT change field names.
// BuildHistory renders the history response. messages is the transcript to
// surface — the caller passes the lossless full transcript read from disk
// (ReadTranscript), NOT the in-memory state.Messages, which is bounded to the
// model's window. Tool-call resolution + session metadata still come from the
// live state.
func BuildHistory(state *SessionState, messages []Message, events []Event, opts ViewOptions) *HistoryResponse {
	if state == nil {
		return &HistoryResponse{
			Messages:     []HistoryMessage{},
			Events:       []HistoryEvent{},
			PendingQueue: []any{},
			InstanceID:   opts.InstanceID,
		}
	}
	state.mu.RLock()
	defer state.mu.RUnlock()

	resp := &HistoryResponse{
		Messages:       make([]HistoryMessage, 0, len(messages)),
		Events:         make([]HistoryEvent, 0, len(events)),
		PendingQueue:   []any{},
		InstanceID:     opts.InstanceID,
		Title:          state.Title,
		Workspace:      state.Workspace,
		Workdir:        state.Workdir,
		LastSeq:        state.LastSeq,
		SnapshotCutoff: 0,
		Tools:          make([]map[string]any, 0, len(state.Tools)),
	}

	for i := range messages {
		m := &messages[i]
		toolCalls := make([]any, 0, len(m.ToolCallIDs))
		for _, callID := range m.ToolCallIDs {
			tc, ok := state.ToolCalls[callID]
			if !ok {
				continue
			}
			entry := map[string]any{
				"id":          tc.CallID,
				"name":        tc.Name,
				"arguments":   tc.Arguments,
				"status":      tc.Status,
				"output":      tc.Output,
				"duration_ms": tc.DurationMs,
			}
			if tc.UnifiedDiff != "" {
				entry["unified_diff"] = tc.UnifiedDiff
			}
			if tc.PreviousContent != "" {
				entry["previous_content"] = tc.PreviousContent
			}
			if tc.NewContent != "" {
				entry["new_content"] = tc.NewContent
			}
			if tc.Error != "" {
				entry["error"] = tc.Error
			}
			toolCalls = append(toolCalls, entry)
		}
		resp.Messages = append(resp.Messages, HistoryMessage{
			Role:        m.Role,
			Content:     m.Content,
			Reasoning:   m.Reasoning,
			ReasoningStartedAt: m.ReasoningStartedAt,
			ReasoningEndedAt:   m.ReasoningEndedAt,
			Seq:         m.Seq,
			StepID:      m.StepID,
			Ts:          unixNanoToISO(m.TsUnixNano),
			ToolCalls:   toolCalls,
			Attachments: m.Attachments,
			Parts:       m.Parts,
		})
	}

	// Populate Tools
	for _, tool := range state.Tools {
		resp.Tools = append(resp.Tools, map[string]any{
			"name":        tool.Name,
			"description": tool.Description,
			"parameters":  tool.Parameters,
		})
	}

	sorted := append([]Event(nil), events...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })

	startSeq := opts.StartSeq
	maxEvents := opts.MaxEvents
	if maxEvents <= 0 {
		maxEvents = len(sorted)
	}

	for i := range sorted {
		ev := &sorted[i]
		if ev.Seq <= startSeq {
			continue
		}
		if len(resp.Events) >= maxEvents {
			resp.EventsHasMore = true
			break
		}
		resp.Events = append(resp.Events, eventToHistoryEvent(ev, opts.IncludePayload))
	}
	resp.EventCount = len(resp.Events)
	resp.EventsTotal = len(sorted)
	if n := len(resp.Events); n > 0 {
		resp.EventsNextSeq = resp.Events[n-1].Seq + 1
	} else {
		resp.EventsNextSeq = startSeq
	}

	return resp
}

func BuildEvents(events []Event, opts ViewOptions) *EventsResponse {
	sorted := append([]Event(nil), events...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })
	maxN := opts.MaxEvents
	if maxN <= 0 {
		maxN = len(sorted)
	}
	out := make([]HistoryEvent, 0, maxN)
	for i := range sorted {
		ev := &sorted[i]
		if ev.Seq <= opts.StartSeq {
			continue
		}
		if len(out) >= maxN {
			break
		}
		out = append(out, eventToHistoryEvent(ev, opts.IncludePayload))
	}
	return &EventsResponse{Events: out, Count: len(out), Total: len(sorted)}
}

func eventToHistoryEvent(ev *Event, includePayload bool) HistoryEvent {
	hev := HistoryEvent{
		ID:            fmt.Sprintf("%s:%d", ev.SessionID, ev.Seq),
		Ts:            unixNanoToISO(ev.TsUnixNano),
		Seq:           ev.Seq,
		Kind:          envelopeKindFor(ev.Type),
		Type:          string(ev.Type),
		CorrelationID: ev.CorrelationID,
	}
	if includePayload {
		hev.Payload = eventPayload(ev)
	}
	return hev
}

func eventPayload(ev *Event) any {
	switch {
	case ev.Message != nil:
		return ev.Message
	case ev.Tool != nil:
		return ev.Tool
	case ev.Approval != nil:
		return ev.Approval
	case ev.Flow != nil:
		return ev.Flow
	case ev.Memory != nil:
		return ev.Memory
	case ev.Workspace != nil:
		return ev.Workspace
	case ev.Agent != nil:
		return ev.Agent
	case ev.Widget != nil:
		return ev.Widget
	case ev.Preview != nil:
		return ev.Preview
	case ev.Todo != nil:
		return ev.Todo
	case ev.Cost != nil:
		return ev.Cost
	case ev.Compact != nil:
		return ev.Compact
	case ev.CtxTokens != nil:
		return ev.CtxTokens
	case ev.CtxCompact != nil:
		return ev.CtxCompact
	case ev.Meta != nil:
		return ev.Meta
	case ev.Error != nil:
		return ev.Error
	case ev.Retry != nil:
		return ev.Retry
	case ev.Turn != nil:
		return ev.Turn
	case ev.Background != nil:
		return ev.Background
	case ev.WorkspaceChanges != nil:
		return ev.WorkspaceChanges
	}
	return nil
}

func envelopeKindFor(t EventType) string {
	switch t {
	case EventError, EventQuarantine:
		return "error"
	case EventSessionStarted, EventSessionEnded, EventSessionInterrupt, EventCompactDone:
		return "system"
	}
	return "session"
}

func isControlEvent(t EventType) bool {
	switch t {
	case EventSessionStarted, EventSessionEnded, EventSessionInterrupt, EventCompactDone, EventQuarantine:
		return true
	}
	return false
}

func unixNanoToISO(ns int64) string {
	if ns == 0 {
		return ""
	}
	return time.Unix(0, ns).UTC().Format(time.RFC3339Nano)
}

func SocketRoomsFor(ev *Event) []string {
	out := make([]string, 0, 3)
	if ev.SessionID != "" {
		out = append(out, "session:"+ev.SessionID)
	}
	if ev.AppID != "" {
		out = append(out, "app:"+ev.AppID)
	}
	if ev.UserID != "" {
		out = append(out, "user:"+ev.UserID)
	}
	return out
}

// PrimaryRoomFor returns the most specific room for an event — session if
// present, app if not, user as last resort. The bridge MUST use this (not
// SocketRoomsFor) to enforce strict session isolation : an event with a
// session_id is routed ONLY to that session's room, never to the parent
// app or user rooms (which would leak the event to all viewers of that app
// or that user's other sessions).
func PrimaryRoomFor(ev *Event) string {
	if ev.SessionID != "" {
		return "session:" + ev.SessionID
	}
	if ev.AppID != "" {
		return "app:" + ev.AppID
	}
	if ev.UserID != "" {
		return "user:" + ev.UserID
	}
	return ""
}

// SubAgentSep namespaces an isolated sub-agent session under its root :
//
//	<root>::agent::<runID>[::agent::<runID>...]   (nested delegation)
//
// It is the single source of truth for the sub-session id shape (runtime's
// subSessionID builds it ; the bridge parses it for agent fan-out).
const SubAgentSep = "::agent::"

// SubAgentSession parses an isolated sub-agent session id. For a sub-session it
// returns the TOP-LEVEL root (everything before the FIRST separator), the
// EMITTING agent's run id (the last segment — the deepest agent in a nested
// tree, the one that actually produced the event), and true. For a plain
// session id it returns ("", "", false).
func SubAgentSession(sid string) (root, runID string, isSub bool) {
	first := strings.Index(sid, SubAgentSep)
	if first < 0 {
		return "", "", false
	}
	last := strings.LastIndex(sid, SubAgentSep)
	root = sid[:first]
	runID = sid[last+len(SubAgentSep):]
	if root == "" || runID == "" {
		return "", "", false
	}
	return root, runID, true
}

// SubAgentAncestors returns the session ids of every ancestor of a sub-agent
// session, from the top-level root down to the immediate parent (the full sid
// itself is NOT included). For "root::agent::r1::agent::r2" it returns
// ["root", "root::agent::r1"] ; for a depth-1 "root::agent::r1" it returns
// ["root"] (identical to the old top-root-only behaviour) ; for a plain session
// id, nil. The bridge fans a sub-session event to EACH ancestor's room so a
// client watching any ancestor — e.g. one drilled into an intermediate
// sub-agent — sees the deeper activity, not only a client on the top root.
func SubAgentAncestors(sid string) []string {
	if !strings.Contains(sid, SubAgentSep) {
		return nil
	}
	parts := strings.Split(sid, SubAgentSep)
	out := make([]string, 0, len(parts)-1)
	cur := parts[0]
	out = append(out, cur)
	for i := 1; i < len(parts)-1; i++ {
		cur = cur + SubAgentSep + parts[i]
		out = append(out, cur)
	}
	return out
}

func generateInstanceID() string {
	return fmt.Sprintf("inst-%d", time.Now().UnixNano())
}

// BuildEnvelope is a convenience wrapper using a default builder. Real
// daemons must use NewEnvelopeBuilder() once at startup and call Build()
// to get a stable instance_id.
func BuildEnvelope(ev *Event) SocketEnvelope {
	return (&EnvelopeBuilder{InstanceID: generateInstanceID()}).Build(ev)
}
