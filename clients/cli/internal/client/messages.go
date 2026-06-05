package client

import (
	"context"
	"fmt"
	"net/url"
)

// PostMessage sends a user message to a session. The daemon persists
// it durably + kicks the turn (RT-1+ async). The assistant reply is
// NOT in this response — subscribe to /events on the Socket.IO bridge
// (CLI-4) OR poll History to see EventAssistantMessage appear.
//
// Endpoint : POST /api/apps/{app_id}/sessions/{sid}/messages
func (c *Client) PostMessage(ctx context.Context, appID, sessionID, content, mode string) (*PostMessageResponse, error) {
	if appID == "" || sessionID == "" {
		return nil, fmt.Errorf("client: appID and sessionID required")
	}
	if content == "" {
		return nil, fmt.Errorf("client: content required")
	}
	path := "/api/apps/" + url.PathEscape(appID) + "/sessions/" + url.PathEscape(sessionID) + "/messages"
	body := PostMessageRequest{Content: content, Role: "user", Mode: mode}
	var out PostMessageResponse
	if err := c.do(ctx, "POST", path, nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
