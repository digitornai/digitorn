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

// Client invokes the daemon's public HTTP API.
type Client struct {
	baseURL string
	jwt     string
	hc      *http.Client

	// pollEvery is how often WaitForReply polls /history. Small enough to feel
	// live, large enough not to hammer the daemon.
	pollEvery time.Duration
}

// Option configures a Client.
type Option func(*Client)

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
	Title           string `json:"title,omitempty"`
	Workdir         string `json:"workdir,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	Message         string `json:"message,omitempty"`
	ClientMessageID string `json:"client_message_id,omitempty"`
	Mode            string `json:"mode,omitempty"`
	EntryAgent      string `json:"entry_agent,omitempty"`
	Context         string `json:"context,omitempty"`
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
	Message         string `json:"message,omitempty"`
	Role            string `json:"role,omitempty"`
	ClientMessageID string `json:"client_message_id,omitempty"`
	Mode            string `json:"mode,omitempty"`
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
	Messages []Message `json:"messages"`
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

// WaitForReply polls history until an assistant message with seq > afterSeq and
// non-empty content appears, or ctx ends (→ ErrReplyTimeout). The caller bounds
// the wait via ctx's deadline.
func (c *Client) WaitForReply(ctx context.Context, appID, sessionID string, afterSeq uint64) (Message, error) {
	t := time.NewTicker(c.pollEvery)
	defer t.Stop()
	for {
		msgs, err := c.History(ctx, appID, sessionID, afterSeq)
		if err != nil {
			// A transient read failure shouldn't abort the wait; keep polling
			// until ctx ends. A permanent one (4xx) does abort.
			if ae, ok := err.(*APIError); ok && !ae.Retryable() {
				return Message{}, err
			}
		} else {
			for i := len(msgs) - 1; i >= 0; i-- {
				m := msgs[i]
				if m.Seq > afterSeq && m.Role == "assistant" && strings.TrimSpace(m.Content) != "" {
					return m, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return Message{}, &ErrReplyTimeout{AfterSeq: afterSeq}
		case <-t.C:
		}
	}
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

// do executes a request, returning (status, body, error). A transport failure
// returns a Status-0 *APIError (retryable).
func (c *Client) do(ctx context.Context, method, path string, body io.Reader, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return 0, nil, &APIError{Op: "request", Message: err.Error()}
	}
	if c.jwt != "" {
		req.Header.Set("Authorization", "Bearer "+c.jwt)
	}
	if u := actAsFromCtx(ctx); u != "" {
		req.Header.Set("X-Act-As-User", u)
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
