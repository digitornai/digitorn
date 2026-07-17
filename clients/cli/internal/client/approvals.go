package client

import (
	"context"
	"fmt"
	"net/url"
)

func (c *Client) ResolveApproval(ctx context.Context, appID, sessionID, approvalID, action, reason string) error {
	if appID == "" || sessionID == "" || approvalID == "" {
		return fmt.Errorf("client: appID, sessionID and approvalID required")
	}
	if action == "" {
		return fmt.Errorf("client: action required")
	}
	path := "/api/apps/" + url.PathEscape(appID) + "/approve"
	body := resolveApprovalRequest{
		SessionID:  sessionID,
		ApprovalID: approvalID,
		Action:     action,
		Reason:     reason,
	}
	return c.do(ctx, "POST", path, nil, body, nil)
}

type resolveApprovalRequest struct {
	SessionID  string `json:"session_id"`
	ApprovalID string `json:"approval_id"`
	Action     string `json:"action"`
	Reason     string `json:"reason,omitempty"`
}
