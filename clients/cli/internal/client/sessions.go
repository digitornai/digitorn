package client

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// ListSessions returns the sessions of an app, sorted by daemon
// (most-recently-updated first). limit/offset are 0-acceptable :
// limit=0 means "daemon default" (currently 50), offset=0 means
// "start of list".
//
// Endpoint : GET /api/apps/{app_id}/sessions
func (c *Client) ListSessions(ctx context.Context, appID string, limit, offset int) (*ListSessionsResponse, error) {
	if appID == "" {
		return nil, fmt.Errorf("client: appID required")
	}
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	var out ListSessionsResponse
	if err := c.do(ctx, "GET", "/api/apps/"+url.PathEscape(appID)+"/sessions", q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SearchSessions performs a server-side filter on title + session_id
// (case-insensitive substring). Empty query returns empty result.
//
// Endpoint : GET /api/apps/{app_id}/sessions/search?q=...
func (c *Client) SearchSessions(ctx context.Context, appID, query string) ([]Session, error) {
	if appID == "" {
		return nil, fmt.Errorf("client: appID required")
	}
	q := url.Values{}
	q.Set("q", query)
	var out struct {
		Sessions []Session `json:"sessions"`
		Total    int       `json:"total"`
	}
	if err := c.do(ctx, "GET", "/api/apps/"+url.PathEscape(appID)+"/sessions/search", q, nil, &out); err != nil {
		return nil, err
	}
	return out.Sessions, nil
}

// CreateSession asks the daemon to mint a new session. The
// SessionID field of the request is OPTIONAL : leave empty to let
// the daemon generate a UUID. Title / Workspace / Workdir are pure
// metadata, surfaced in list views.
//
// Endpoint : POST /api/apps/{app_id}/sessions
func (c *Client) CreateSession(ctx context.Context, appID string, req CreateSessionRequest) (*CreateSessionResponse, error) {
	if appID == "" {
		return nil, fmt.Errorf("client: appID required")
	}
	var out CreateSessionResponse
	if err := c.do(ctx, "POST", "/api/apps/"+url.PathEscape(appID)+"/sessions", nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteSession permanently removes the session : its events file,
// any snapshot, and the in-memory state entry. Irrecoverable.
//
// Endpoint : DELETE /api/apps/{app_id}/sessions/{sid}
func (c *Client) DeleteSession(ctx context.Context, appID, sessionID string) error {
	if appID == "" || sessionID == "" {
		return fmt.Errorf("client: appID and sessionID required")
	}
	path := "/api/apps/" + url.PathEscape(appID) + "/sessions/" + url.PathEscape(sessionID)
	return c.do(ctx, "DELETE", path, nil, nil, nil)
}

// History fetches the projected message list + raw events for a
// session. since/limit gate the events slice : since=0 means "from
// the start", limit=0 means "all".
//
// Endpoint : GET /api/apps/{app_id}/sessions/{sid}/history?since=N&limit=M
func (c *Client) History(ctx context.Context, appID, sessionID string, since uint64, limit int) (*HistoryResponse, error) {
	if appID == "" || sessionID == "" {
		return nil, fmt.Errorf("client: appID and sessionID required")
	}
	q := url.Values{}
	if since > 0 {
		q.Set("since", strconv.FormatUint(since, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/api/apps/" + url.PathEscape(appID) + "/sessions/" + url.PathEscape(sessionID) + "/history"
	var out HistoryResponse
	if err := c.do(ctx, "GET", path, q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AbortTurn sends EventSessionInterrupt — the daemon honors it at the
// next turn-loop checkpoint (RT-6 will make this propagate to the
// LLM call cancellation).
//
// Endpoint : POST /api/apps/{app_id}/sessions/{sid}/abort
func (c *Client) AbortTurn(ctx context.Context, appID, sessionID string) error {
	if appID == "" || sessionID == "" {
		return fmt.Errorf("client: appID and sessionID required")
	}
	path := "/api/apps/" + url.PathEscape(appID) + "/sessions/" + url.PathEscape(sessionID) + "/abort"
	return c.do(ctx, "POST", path, nil, nil, nil)
}
