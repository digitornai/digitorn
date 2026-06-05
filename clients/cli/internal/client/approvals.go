package client

import (
	"context"
	"fmt"
	"net/url"
)

// ResolveApproval answers a pending tool-call approval. action is
// "approved" or "denied" — the daemon maps {deny, denied, reject} to a
// denial and everything else to a grant. reason is optional free text
// surfaced in the session timeline.
//
// This deliberately uses REST, not the Socket.IO resolve_approval
// event : only the REST path signals the runtime's approval registry,
// which is what actually unblocks the suspended turn. The socket event
// records a durable row but leaves the turn waiting until timeout.
//
// Endpoint : POST /api/apps/{app_id}/approve
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
