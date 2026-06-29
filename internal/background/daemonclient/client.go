// Package daemonclient reaches the daemon ONLY over its public HTTP API — the
// same endpoints any external client uses (POST /sessions, POST /messages,
// GET /history). It imports nothing from internal/server or internal/runtime,
// so the background service stays fully isolated: the daemon is a black box
// addressed by URL + a service JWT.
//
// The client is the BG-3 "invocation primitive": given a target app + a message
// it launches (or feeds) an agentic session and, optionally, waits for the
// agent's reply. Higher-level channel/session strategies (per_event / shared /
// template) live above it in BG-4.
package daemonclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
)

// streamStartGrace bounds how long StreamReplies waits for the (async) turn to
// actually start before giving up — so a posted message whose turn never fires
// doesn't hang the relay forever.
const streamStartGrace = 30 * time.Second

// Client invokes the daemon's public HTTP API.
type Client struct {
	baseURL string
	jwt     string
	hc      *http.Client

	// userToken, when set, returns a valid per-user access token for a request
	// made on behalf of that user (X-Act-As-User). When it yields a token the
	// request authenticates AS the user (real JWT) — so a background turn carries
	// a UserJWT the LLM gateway accepts — instead of the service-token + act-as
	// impersonation. nil or "" → falls back to the service JWT + act-as.
	userToken func(ctx context.Context, userID string) (string, error)

	// pollEvery is how often WaitForReply polls /history. Small enough to feel
	// live, large enough not to hammer the daemon.
	pollEvery time.Duration
}

// Option configures a Client.
type Option func(*Client)

// WithUserTokenProvider wires a per-user access-token source (the userauth
// manager) so requests on behalf of a user carry that user's real JWT.
func WithUserTokenProvider(f func(ctx context.Context, userID string) (string, error)) Option {
	return func(c *Client) { c.userToken = f }
}

// SetUserTokenProvider sets the per-user token source after construction (the
// manager needs the DB, opened later in service.New).
func (c *Client) SetUserTokenProvider(f func(ctx context.Context, userID string) (string, error)) {
	c.userToken = f
}

// WithHTTPClient overrides the underlying *http.Client (timeouts, transport).
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.hc = hc } }

// WithPollInterval sets how often WaitForReply polls the history endpoint.
func WithPollInterval(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.pollEvery = d
		}
	}
}

// New builds a client against baseURL (e.g. http://127.0.0.1:8000) authenticating
// with the given service JWT (may be empty when the daemon runs in dev mode).
func New(baseURL, jwt string, opts ...Option) *Client {
	c := &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		jwt:       jwt,
		hc:        &http.Client{Timeout: 30 * time.Second},
		pollEvery: 500 * time.Millisecond,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// ── Wire types (mirror the daemon's request/response shapes exactly) ──────────

// CreateSessionRequest is the body of POST /api/apps/{app_id}/sessions. Setting
// Message creates the session AND starts the first turn in one round-trip.
type CreateSessionRequest struct {
	Title           string    `json:"title,omitempty"`
	Workdir         string    `json:"workdir,omitempty"`
	SessionID       string    `json:"session_id,omitempty"`
	Message         string `json:"message,omitempty"`
	ClientMessageID string `json:"client_message_id,omitempty"`
	Mode            string `json:"mode,omitempty"`
	EntryAgent      string `json:"entry_agent,omitempty"`
	Context         string `json:"context,omitempty"`
	Model           string `json:"model,omitempty"`
	Attachments     []BlobRef `json:"attachments,omitempty"`
	// TriggerEvent is the structured inbound event (channels scope) attached to
	// the inline first message so flow nodes can read {{event.payload.*}}.
	TriggerEvent map[string]any `json:"trigger_event,omitempty"`
}

// BlobRef references a stored blob by content hash — the daemon's BlobRef wire shape.
// A message attaches these and the daemon resolves them into vision/audio content.
type BlobRef struct {
	Hash string `json:"hash"`
	Mime string `json:"mime"`
	Size int64  `json:"size"`
}

// CreateSessionResponse is the daemon's reply to a create call.
type CreateSessionResponse struct {
	SessionID    string `json:"session_id"`
	AppID        string `json:"app_id"`
	UserID       string `json:"user_id"`
	Seq          uint64 `json:"seq"`
	Workdir      string `json:"workdir"`
	FirstMessage struct {
		Seq uint64 `json:"seq"`
	} `json:"first_message"`
}

// PostMessageRequest is the body of POST .../messages.
type PostMessageRequest struct {
	Message         string         `json:"message,omitempty"`
	Role            string         `json:"role,omitempty"`
	ClientMessageID string         `json:"client_message_id,omitempty"`
	Mode            string         `json:"mode,omitempty"`
	Attachments     []BlobRef      `json:"attachments,omitempty"`
	TriggerEvent    map[string]any `json:"trigger_event,omitempty"`
}

// PostMessageResponse is the daemon's reply to a posted message.
type PostMessageResponse struct {
	SessionID string `json:"session_id"`
	Seq       uint64 `json:"seq"`
	Role      string `json:"role"`
}

// Message is one projected history entry (subset we consume).
type Message struct {
	Seq     uint64 `json:"seq"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

type historyResponse struct {
	Messages   []Message      `json:"messages"`
	Events     []historyEvent `json:"events"`
	TurnActive bool           `json:"turn_active"`
}

// historyEvent is one entry of the session's seq-ordered event stream (the subset
// we relay): assistant messages and tool results. Used by StreamReplies to surface
// the full agentic loop in a channel, not just the final answer.
type historyEvent struct {
	Seq     uint64 `json:"seq"`
	Type    string `json:"type"`
	Payload struct {
		Content    string `json:"content"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		DurationMs int64  `json:"duration_ms"`
	} `json:"payload"`
}

// StreamItem is one piece of an agentic turn to surface in the channel: an assistant
// message (Kind "message") or a finished tool call (Kind "tool").
type StreamItem struct {
	Seq  uint64
	Kind string
	Text string
}

// ── Errors ────────────────────────────────────────────────────────────────

// APIError carries a non-2xx daemon response. Status 0 means a transport error
// (no HTTP response was received).
type APIError struct {
	Op      string
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("%s: transport error: %s", e.Op, e.Message)
	}
	return fmt.Sprintf("%s: daemon %d %s: %s", e.Op, e.Status, e.Code, e.Message)
}

// Retryable reports whether the failure is transient (worth a backoff retry):
// transport errors and 408/425/429/5xx. A 4xx (other than those) is a permanent
// client/contract error — retrying would just fail the same way.
func (e *APIError) Retryable() bool {
	switch e.Status {
	case 0, http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	default:
		return e.Status >= 500
	}
}

// ErrReplyTimeout is returned by WaitForReply when ctx ends before an assistant
// reply lands. The user message WAS delivered and the turn runs server-side, so
// callers must NOT re-post on this error (that would double-send).
type ErrReplyTimeout struct{ AfterSeq uint64 }

func (e *ErrReplyTimeout) Error() string {
	return fmt.Sprintf("timed out waiting for assistant reply after seq %d", e.AfterSeq)
}

// ── Calls ─────────────────────────────────────────────────────────────────

// SessionExists reports whether a session id is already present (200) or not
// (404). Any other status is surfaced as an APIError.
func (c *Client) SessionExists(ctx context.Context, appID, sessionID string) (bool, error) {
	path := fmt.Sprintf("/api/apps/%s/sessions/%s", url(appID), url(sessionID))
	status, _, err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound {
		return false, nil
	}
	if status >= 200 && status < 300 {
		return true, nil
	}
	return false, &APIError{Op: "session_exists", Status: status}
}

// CreateSession creates a session (optionally with an inline first message).
func (c *Client) CreateSession(ctx context.Context, appID string, req CreateSessionRequest) (CreateSessionResponse, error) {
	var out CreateSessionResponse
	path := fmt.Sprintf("/api/apps/%s/sessions", url(appID))
	if err := c.doJSON(ctx, http.MethodPost, path, req, &out, "create_session"); err != nil {
		return CreateSessionResponse{}, err
	}
	return out, nil
}

// SetModel overrides a session's entry-agent model (PUT .../model). Idempotent —
// safe to call on every launch so a SHARED/existing session also picks up the
// trigger's configured model (the create path sets it inline for new sessions).
func (c *Client) SetModel(ctx context.Context, appID, sessionID, model string) error {
	path := fmt.Sprintf("/api/apps/%s/sessions/%s/model", url(appID), url(sessionID))
	return c.doJSON(ctx, http.MethodPut, path, map[string]string{"model": model}, nil, "set_model")
}

// UploadBlob streams bytes (with their mime type) to the session's blob store and
// returns the BlobRef a message can attach. The daemon resolves the ref into
// vision/audio content for the model.
func (c *Client) UploadBlob(ctx context.Context, appID, mime string, data []byte) (BlobRef, error) {
	path := fmt.Sprintf("/api/apps/%s/blobs", url(appID))
	status, raw, err := c.do(ctx, http.MethodPost, path, bytes.NewReader(data), map[string]string{"Content-Type": mime})
	if err != nil {
		return BlobRef{}, err
	}
	if status < 200 || status >= 300 {
		return BlobRef{}, &APIError{Op: "upload_blob", Status: status, Code: codeOf(raw), Message: messageOf(raw)}
	}
	var out BlobRef
	if e := json.Unmarshal(raw, &out); e != nil {
		return BlobRef{}, &APIError{Op: "upload_blob", Status: status, Message: "decode: " + e.Error()}
	}
	return out, nil
}

// PostMessage appends a message to an existing session, triggering a turn.
func (c *Client) PostMessage(ctx context.Context, appID, sessionID string, req PostMessageRequest) (PostMessageResponse, error) {
	var out PostMessageResponse
	path := fmt.Sprintf("/api/apps/%s/sessions/%s/messages", url(appID), url(sessionID))
	if err := c.doJSON(ctx, http.MethodPost, path, req, &out, "post_message"); err != nil {
		return PostMessageResponse{}, err
	}
	return out, nil
}

// Abort cancels the in-flight turn of a session (the daemon's POST .../abort).
// Used for voice barge-in so a turn the caller interrupted stops generating.
func (c *Client) Abort(ctx context.Context, appID, sessionID string) error {
	path := fmt.Sprintf("/api/apps/%s/sessions/%s/abort", url(appID), url(sessionID))
	return c.doJSON(ctx, http.MethodPost, path, nil, nil, "abort")
}

// History returns projected messages with seq > since.
func (c *Client) History(ctx context.Context, appID, sessionID string, since uint64) ([]Message, error) {
	path := fmt.Sprintf("/api/apps/%s/sessions/%s/history?since=%d", url(appID), url(sessionID), since)
	var out historyResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out, "history"); err != nil {
		return nil, err
	}
	return out.Messages, nil
}

// WaitForReply polls history until the agentic turn ENDS (durable turn_ended event)
// and returns the LAST non-empty assistant message it produced. Waiting for the whole
// turn — not the first assistant message — is what makes a tool-using turn deliver its
// FINAL answer to the channel ("here are the files") instead of a mid-loop preamble
// ("let me look…").
//
// Completion is keyed on the turn_ended event, NOT turn_active: for an async
// background-driven turn, turn_active in /history reads false the whole time, so
// gating on !turn_active would return the first preamble before the tools even run
// (the same trap that StreamReplies hit). The reply text is read from assistant_message
// EVENTS rather than the projected Messages slice so a lagging projection can't make us
// deliver stale or empty content at turn_ended. A startup grace bounds the wait if the
// async turn never starts. Transient read errors are retried; a permanent 4xx aborts;
// ctx end returns whatever final reply we have, else ErrReplyTimeout.
func (c *Client) WaitForReply(ctx context.Context, appID, sessionID string, afterSeq uint64) (Message, error) {
	t := time.NewTicker(c.pollEvery)
	defer t.Stop()
	path := fmt.Sprintf("/api/apps/%s/sessions/%s/history?since=%d", url(appID), url(sessionID), afterSeq)
	var last Message
	have := false
	started := false
	startDeadline := time.Now().Add(streamStartGrace)
	for {
		var resp historyResponse
		err := c.doJSON(ctx, http.MethodGet, path, nil, &resp, "history")
		if err != nil {
			if ae, ok := err.(*APIError); ok && !ae.Retryable() {
				return Message{}, err
			}
		} else {
			done := false
			for _, e := range resp.Events {
				if e.Seq <= afterSeq {
					continue
				}
				switch e.Type {
				case "assistant_message":
					started = true
					if txt := strings.TrimSpace(e.Payload.Content); txt != "" {
						last, have = Message{Seq: e.Seq, Role: "assistant", Content: txt}, true
					}
				case "tool_call", "tool_result", "assistant_delta", "turn_started", "turn_phase_changed":
					// The turn is producing — but session_started / user_message /
					// model_changed must NOT count, or we'd conclude "done" before it runs.
					started = true
				case "turn_ended":
					started, done = true, true
				}
			}
			if done {
				if have {
					return last, nil
				}
				return Message{}, &ErrReplyTimeout{AfterSeq: afterSeq} // turn ended with no assistant text
			}
			if !started && time.Now().After(startDeadline) {
				return Message{}, &ErrReplyTimeout{AfterSeq: afterSeq} // the turn never started
			}
		}
		select {
		case <-ctx.Done():
			if have {
				return last, nil // deadline mid-turn: deliver the best reply we have
			}
			return Message{}, &ErrReplyTimeout{AfterSeq: afterSeq}
		case <-t.C:
		}
	}
}

// Approval is one pending human-in-the-loop decision the daemon is blocked on: a
// gated tool awaiting approval (Kind=="tool_call") or an ask_user question
// (Kind=="question"). It mirrors the daemon's projected ApprovalState (subset).
// For a tool_call, ToolName/ToolParams/RiskLevel describe what the agent wants to
// run; for a question, Reason is the question text and Payload carries the shape
// (choices/allow_custom/placeholder/multiline).
type Approval struct {
	ApprovalID string         `json:"id"`
	Kind       string         `json:"kind"`
	Status     string         `json:"status"`
	Reason     string         `json:"reason,omitempty"`
	AgentID    string         `json:"agent_id,omitempty"`
	ToolName   string         `json:"tool_name,omitempty"`
	ToolParams map[string]any `json:"tool_params,omitempty"`
	RiskLevel  string         `json:"risk_level,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
}

// stateApprovals decodes just the approvals map out of GET /state's SessionSnapshot.
type stateApprovals struct {
	Approvals map[string]Approval `json:"approvals"`
}

// PendingApprovals returns the session's currently-pending approvals (full detail),
// read from the session snapshot. HTTP-only: an out-of-process client (the
// background service) learns what the blocked turn is asking for without a socket.
func (c *Client) PendingApprovals(ctx context.Context, appID, sessionID string) ([]Approval, error) {
	path := fmt.Sprintf("/api/apps/%s/sessions/%s/state", url(appID), url(sessionID))
	var snap stateApprovals
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &snap, "state"); err != nil {
		return nil, err
	}
	out := make([]Approval, 0, len(snap.Approvals))
	for id, ap := range snap.Approvals {
		if ap.Status != "" && ap.Status != "pending" {
			continue
		}
		if ap.ApprovalID == "" {
			ap.ApprovalID = id
		}
		out = append(out, ap)
	}
	return out, nil
}

// ResolveApproval resolves a pending approval: action is "grant" (approve / answer)
// or "deny" (reject); reason carries an ask_user answer or a refusal note. This is
// the daemon's POST /approve — it unblocks the parked turn via the approval registry.
func (c *Client) ResolveApproval(ctx context.Context, appID, sessionID, approvalID, action, reason string) error {
	path := fmt.Sprintf("/api/apps/%s/approve", url(appID))
	body := map[string]string{
		"session_id":  sessionID,
		"approval_id": approvalID,
		"action":      action,
		"reason":      reason,
	}
	return c.doJSON(ctx, http.MethodPost, path, body, nil, "approve")
}

// StreamReplies surfaces the FULL agentic turn live: it polls the session's event
// stream and hands each new assistant message and finished tool call to onItem, in
// seq order, until the turn settles (turn_active=false). This is what lets a channel
// show "I'll analyze… → 🔧 filesystem.glob → here's the report" instead of only the
// final answer. Transient read errors are retried; a permanent 4xx aborts; ctx end
// stops the stream.
func (c *Client) StreamReplies(ctx context.Context, appID, sessionID string, afterSeq uint64, onItem func(StreamItem)) error {
	t := time.NewTicker(c.pollEvery)
	defer t.Stop()
	since := afterSeq
	// The turn is triggered ASYNC after the message is posted, so the first poll may
	// catch turn_active=false BEFORE the turn starts. Only conclude the turn is done
	// once we've actually seen it active (or produced events) — else a startup grace
	// expires so we never hang if the turn truly never starts.
	started := false
	startDeadline := time.Now().Add(streamStartGrace)
	for {
		path := fmt.Sprintf("/api/apps/%s/sessions/%s/history?since=%d", url(appID), url(sessionID), since)
		var resp historyResponse
		err := c.doJSON(ctx, http.MethodGet, path, nil, &resp, "history")
		if err != nil {
			if ae, ok := err.(*APIError); ok && !ae.Retryable() {
				return err
			}
		} else {
			done := false
			for _, e := range resp.Events {
				if e.Seq <= since {
					continue
				}
				since = e.Seq
				switch e.Type {
				case "assistant_message":
					started = true
					if txt := strings.TrimSpace(e.Payload.Content); txt != "" {
						onItem(StreamItem{Seq: e.Seq, Kind: "message", Text: txt})
					}
				case "tool_result":
					started = true
					onItem(StreamItem{Seq: e.Seq, Kind: "tool", Text: toolLine(e.Payload.Name, e.Payload.Status)})
				case "tool_call", "assistant_delta", "turn_started", "turn_phase_changed":
					// The turn is producing — but session_started / user_message /
					// model_changed (pre-existing on a since=0 scan) must NOT count, or
					// we'd conclude "done" before the turn even runs.
					started = true
				case "turn_ended":
					started = true
					done = true
				}
			}
			// Completion is the durable turn_ended event. turn_active in /history is NOT
			// a reliable "running" signal for an async background turn (it can read false
			// the whole time), so relying on it ends the stream before the reply lands.
			if done {
				return nil
			}
			if !started && time.Now().After(startDeadline) {
				return nil // the turn never started within the grace window
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// toolLine renders a compact one-line tool-activity marker for the channel.
func toolLine(name, status string) string {
	if name == "" {
		name = "tool"
	}
	mark := "✓"
	if status != "completed" && status != "" {
		mark = "✗"
	}
	return "🔧 " + name + " " + mark
}

// ── HTTP plumbing ───────────────────────────────────────────────────────────

func (c *Client) doJSON(ctx context.Context, method, path string, body, out any, op string) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return &APIError{Op: op, Message: err.Error()}
		}
		rdr = bytes.NewReader(b)
	}
	status, raw, err := c.do(ctx, method, path, rdr, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err // already an *APIError (transport)
	}
	if status < 200 || status >= 300 {
		return &APIError{Op: op, Status: status, Code: codeOf(raw), Message: messageOf(raw)}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return &APIError{Op: op, Status: status, Message: "decode: " + err.Error()}
		}
	}
	return nil
}

// WithActAs returns a context that makes the client impersonate the given end-user
// (X-Act-As-User) on EVERY request under it — for callers that must touch a
// user-owned session across several calls (e.g. polling/resolving approvals while a
// turn runs). Empty user → ctx unchanged (the service owns the session).
func WithActAs(ctx context.Context, user string) context.Context { return withActAs(ctx, user) }

type actAsKey struct{}

// withActAs marks a context so every request made under it carries X-Act-As-User :
// the daemon then owns any session created / touched as that end-user (the service
// JWT must carry the impersonation grant). One Launch = one owner, propagated to all
// its sub-calls (exists / create / message / history).
func withActAs(ctx context.Context, user string) context.Context {
	if user == "" {
		return ctx
	}
	return context.WithValue(ctx, actAsKey{}, user)
}

func actAsFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(actAsKey{}).(string)
	return v
}

// PieceAuth fetches the configured _ap_auth wire for an installed piece,
// acting as the trigger owner so the background runtime uses THAT user's
// credentials. Returned as a generic map (sent to the bridge as-is).
func (c *Client) PieceAuth(ctx context.Context, owner, pieceName string) (map[string]any, error) {
	if owner != "" {
		ctx = withActAs(ctx, owner)
	}
	status, raw, err := c.do(ctx, http.MethodGet,
		"/api/pieces/"+neturl.PathEscape(pieceName)+"/bridge-auth", nil, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &APIError{Op: "piece_auth", Status: status, Message: string(raw)}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &APIError{Op: "piece_auth", Message: err.Error()}
	}
	return out, nil
}

// AppChannelSecret fetches the installation's stored value for one channel
// secret key (a bot token pasted in the UI), so BuildPlan can resolve a
// {{secret.X}} placeholder at arm time. Returns "" (no error) when unset.
func (c *Client) AppChannelSecret(ctx context.Context, appID, key string) (string, error) {
	status, raw, err := c.do(ctx, http.MethodGet,
		"/api/apps/"+neturl.PathEscape(appID)+"/channel-secret?key="+neturl.QueryEscape(key), nil, nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", &APIError{Op: "channel_secret", Status: status, Message: string(raw)}
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", &APIError{Op: "channel_secret", Message: err.Error()}
	}
	return out.Value, nil
}

// do executes a request, returning (status, body, error). A transport failure
// returns a Status-0 *APIError (retryable).
func (c *Client) do(ctx context.Context, method, path string, body io.Reader, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return 0, nil, &APIError{Op: "request", Message: err.Error()}
	}
	// On behalf of a user: prefer that user's real access token (so a background
	// turn carries a UserJWT the gateway accepts). Fall back to the service JWT +
	// X-Act-As-User impersonation when no per-user token is available.
	authed := false
	if u := actAsFromCtx(ctx); u != "" {
		if c.userToken != nil {
			if tok, terr := c.userToken(ctx, u); terr == nil && tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
				authed = true
			}
		}
		if !authed {
			req.Header.Set("X-Act-As-User", u)
		}
	}
	if !authed && c.jwt != "" {
		req.Header.Set("Authorization", "Bearer "+c.jwt)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, &APIError{Op: "transport", Message: err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

// codeOf / messageOf extract the daemon's {error,code,message} error envelope
// (best-effort; falls back to the raw body).
func codeOf(raw []byte) string {
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(raw, &e)
	return e.Code
}

func messageOf(raw []byte) string {
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	if json.Unmarshal(raw, &e) == nil {
		switch {
		case e.Message != "":
			return e.Message
		case e.Error != "":
			return e.Error
		case e.Detail != "":
			return e.Detail
		}
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// url path-escapes a single path segment (app ids / session ids are slugs or
// uuids, but escape defensively against a hostile id).
func url(seg string) string { return neturl.PathEscape(seg) }
