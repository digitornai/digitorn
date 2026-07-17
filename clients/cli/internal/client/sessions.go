package client

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

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

func (c *Client) DeleteSession(ctx context.Context, appID, sessionID string) error {
	if appID == "" || sessionID == "" {
		return fmt.Errorf("client: appID and sessionID required")
	}
	path := "/api/apps/" + url.PathEscape(appID) + "/sessions/" + url.PathEscape(sessionID)
	return c.do(ctx, "DELETE", path, nil, nil, nil)
}

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

func (c *Client) AbortTurn(ctx context.Context, appID, sessionID string) error {
	if appID == "" || sessionID == "" {
		return fmt.Errorf("client: appID and sessionID required")
	}
	path := "/api/apps/" + url.PathEscape(appID) + "/sessions/" + url.PathEscape(sessionID) + "/abort"
	return c.do(ctx, "POST", path, nil, nil, nil)
}
