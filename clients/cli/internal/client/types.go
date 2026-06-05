// Package client speaks the digitorn daemon's public wire protocol :
// REST under /api/* and Socket.IO under /socket.io/. Every type here
// is the EXACT JSON shape returned by the daemon, mirrored locally so
// this module has zero Go-level coupling with the daemon module.
//
// When the daemon's wire format changes, this package is the canary :
// failing tests here mean we forgot to either bump the wire-version
// or migrate the client. Wire-versioning is enforced at integration
// test time, not unit test time.
package client

import "time"

// App mirrors the daemon's appmgr.App JSON shape. Returned by GET
// /api/apps, GET /api/apps/{app_id}, POST /api/apps/install (in the
// install response envelope).
type App struct {
	AppID       string    `json:"app_id"`
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Description string    `json:"description,omitempty"`
	Category    string    `json:"category,omitempty"`
	Author      string    `json:"author,omitempty"`
	Icon        string    `json:"icon,omitempty"`
	Color       string    `json:"color,omitempty"`
	Enabled     bool      `json:"enabled"`
	BYOK        bool      `json:"byok"`
	InstalledAt time.Time `json:"installed_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// InstallResponse is the body of POST /api/apps/install. The daemon
// returns the resolved install_dir + source for traceability ; we
// expose it on the App.
type InstallResponse struct {
	AppID      string `json:"app_id"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	Source     string `json:"source"`
	InstallDir string `json:"install_dir"`
	Enabled    bool   `json:"enabled"`
	BYOK       bool   `json:"byok"`
}

// ListAppsResponse is the wrapper for GET /api/apps. The daemon
// returns {apps: [...], count: N} ; we expose just the slice in the
// client API and read count for logging.
type ListAppsResponse struct {
	Apps  []App `json:"apps"`
	Count int   `json:"count"`
}

// Session is a thin view of the session metadata returned by GET
// /api/apps/{app_id}/sessions. Not every field is filled by every
// endpoint — see SessionDetail for the full view returned by GET
// /api/apps/{app_id}/sessions/{sid}.
type Session struct {
	SessionID   string `json:"session_id"`
	AppID       string `json:"app_id"`
	UserID      string `json:"user_id,omitempty"`
	Title       string `json:"title,omitempty"`
	LastSeq     uint64 `json:"last_seq"`
	EventCount  uint64 `json:"event_count"`
	StartedAt   string `json:"started_at,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	Workspace   string `json:"workspace,omitempty"`
	Workdir     string `json:"workdir,omitempty"`
	Closed      bool   `json:"closed,omitempty"`
	Interrupted bool   `json:"interrupted,omitempty"`
	// Preview is a short snippet of the session's first user message (daemon-
	// provided), used to label the session by topic instead of a generic title.
	Preview string `json:"preview,omitempty"`
}

// ListSessionsResponse is the wrapper for GET .../sessions. Daemon
// returns {sessions: [...], total, limit, offset}.
type ListSessionsResponse struct {
	Sessions []Session `json:"sessions"`
	Total    int       `json:"total"`
	Limit    int       `json:"limit"`
	Offset   int       `json:"offset"`
}

// CreateSessionRequest is the body of POST /api/apps/{app_id}/sessions.
type CreateSessionRequest struct {
	Title     string `json:"title,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Workdir   string `json:"workdir,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// CreateSessionResponse is the daemon's reply : the freshly minted
// session_id + the seq number of the EventSessionStarted event +
// echoed metadata.
type CreateSessionResponse struct {
	SessionID  string `json:"session_id"`
	AppID      string `json:"app_id"`
	UserID     string `json:"user_id"`
	Seq        uint64 `json:"seq"`
	StartedAt  string `json:"started_at"`
	Title      string `json:"title"`
	Workspace  string `json:"workspace"`
	Workdir    string `json:"workdir"`
	InstanceID string `json:"instance_id"`
}

// Message is one entry in the projected session history. Returned by
// GET /api/apps/{app_id}/sessions/{sid}/history (which also includes
// raw events ; here we surface just the messages for the TUI).
type Message struct {
	Seq         uint64    `json:"seq"`
	Role        string    `json:"role"`
	Content     string    `json:"content"`
	TsUnixNano  int64     `json:"ts"`
	ToolCallIDs []string  `json:"tool_call_ids,omitempty"`
	Attachments []BlobRef `json:"attachments,omitempty"`

	// CLI-only display fields for inline tool-call chips (never from the
	// wire). A "tool" role chip is tracked by CallID and updated in place
	// when its tool_result lands : Status flips running→completed/errored,
	// DurationMs fills in, and ToolOutput captures a short result preview.
	// ToolArg is a one-line hint of the call's key argument.
	CallID     string `json:"-"`
	Status     string `json:"-"`
	DurationMs int64  `json:"-"`
	ToolArg    string `json:"-"`
	ToolOutput string `json:"-"`
	// ToolDiff is the unified diff of a file mutation (edit/write), carried
	// client-side from the tool_result so the chip renders a coloured preview.
	// Empty for non-mutating tools.
	ToolDiff string `json:"-"`
	// ToolCount is a live count of the tools a delegated sub-agent has run so
	// far, derived client-side from its fanned tool_call events. Shown on the
	// "agent" chip while it works. Zero for normal tool chips.
	ToolCount int `json:"-"`
	// Reasoning is the agent's thinking-mode trace for this assistant message,
	// rendered as a collapsed 💭 block above the reply.
	Reasoning string `json:"-"`
}

// BlobRef is the daemon's attachment descriptor.
type BlobRef struct {
	Hash string `json:"hash"`
	Mime string `json:"mime"`
	Size int64  `json:"size"`
}

// HistoryResponse is the wrapper for GET .../history. The daemon
// returns messages + raw events + pending_queue ; the TUI only needs
// messages at first.
type HistoryResponse struct {
	Messages     []Message `json:"messages"`
	Events       []Event   `json:"events,omitempty"`
	PendingQueue []any     `json:"pending_queue"`
}

// Event is the raw session event. Used by the streaming layer
// (Socket.IO, CLI-4) and by the history endpoint as the underlying
// audit trail. The shape mirrors sessionstore.Event but the payloads
// are decoded lazily as map[string]any to avoid mirroring every
// payload type here.
type Event struct {
	Seq           uint64         `json:"seq"`
	Type          string         `json:"type"`
	TsUnixNano    int64          `json:"ts"`
	SessionID     string         `json:"session_id"`
	AppID         string         `json:"app_id,omitempty"`
	UserID        string         `json:"user_id,omitempty"`
	CorrelationID string         `json:"correlation_id,omitempty"`
	Message       map[string]any `json:"message,omitempty"`
	Tool          map[string]any `json:"tool,omitempty"`
	Approval      map[string]any `json:"approval,omitempty"`
	Turn          map[string]any `json:"turn,omitempty"`
	Cost          map[string]any `json:"cost,omitempty"`
	Meta          map[string]any `json:"meta,omitempty"`
	Error         map[string]any `json:"error,omitempty"`
	Extra         map[string]any `json:"-"`
}

// PostMessageRequest is the body of POST .../messages. We always send
// role="user" implicitly (daemon defaults to user when empty).
type PostMessageRequest struct {
	Content string `json:"content"`
	Role    string `json:"role,omitempty"`
	// Mode is the composer mode id selected for this message (runtime.modes).
	// Empty → the daemon falls back to the session's sticky mode / app default.
	Mode string `json:"mode,omitempty"`
}

// Mode is one composer mode declared by an app (runtime.modes), surfaced so the
// CLI can show a switcher. ID is the YAML key sent back as PostMessageRequest.Mode.
type Mode struct {
	ID          string
	Label       string
	Description string
	Icon        string
}

// PostMessageResponse is the 201 reply : seq of the persisted user
// message + role + timestamp. The assistant reply arrives separately
// via Socket.IO stream OR a subsequent history poll.
type PostMessageResponse struct {
	SessionID string `json:"session_id"`
	Seq       uint64 `json:"seq"`
	Role      string `json:"role"`
	TsRFC3339 string `json:"ts"`
}

// APIError is the canonical error shape : the daemon always returns
// {"error":"<code>","message":"<human>"} for non-2xx responses. The
// client wraps non-2xx in this struct + the HTTP status.
type APIError struct {
	StatusCode int    `json:"-"`
	Code       string `json:"error"`
	Message    string `json:"message"`
}

func (e *APIError) Error() string {
	if e.Code != "" && e.Message != "" {
		return e.Code + ": " + e.Message
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Code != "" {
		return e.Code
	}
	return "api error"
}
