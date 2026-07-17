package server

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/digitornai/digitorn/internal/runtime/context/meta"
	"github.com/digitornai/digitorn/internal/runtime/policy/approval"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

type askUserBridge struct {
	store  *sessionstore.Bus
	reg    *approval.Registry
	logger *slog.Logger
}

const defaultAskTimeout = 3600 * time.Second

func (b *askUserBridge) Ask(ctx context.Context, req meta.AskUserRequest) (string, error) {
	if b == nil || b.store == nil || b.reg == nil {
		return "", errors.New("ask_user bridge not wired")
	}
	if req.SessionID == "" {
		return "", errors.New("ask_user: no session to address the question to")
	}

	id := uuid.NewString()
	payload := map[string]any{}
	if req.Content != "" {
		payload["content"] = req.Content
	}
	if len(req.Choices) > 0 {
		payload["choices"] = req.Choices
		payload["allow_multiple"] = req.AllowMultiple
		if req.MinSelect > 0 {
			payload["min_select"] = req.MinSelect
		}
		if req.MaxSelect > 0 {
			payload["max_select"] = req.MaxSelect
		}
	}
	if len(req.Form) > 0 {
		payload["form"] = req.Form
	}
	if len(req.Choices) > 0 || len(req.Form) > 0 {
		payload["allow_custom"] = req.AllowCustom
	}
	if req.Default != "" {
		payload["default"] = req.Default
	}
	if req.Placeholder != "" {
		payload["placeholder"] = req.Placeholder
	}
	if req.Multiline {
		payload["multiline"] = true
	}
	if len(payload) == 0 {
		payload = nil
	}
	pending := b.reg.Arm(id)

	if _, err := b.store.AppendDurable(ctx, sessionstore.Event{
		Type:      sessionstore.EventApprovalRequest,
		SessionID: req.SessionID,
		AppID:     req.AppID,
		UserID:    req.UserID,
		Approval: &sessionstore.ApprovalPayload{
			ID:      id,
			Kind:    "question",
			Status:  "pending",
			Reason:  req.Question,
			Payload: payload,
		},
	}); err != nil {
		return "", err
	}

	timeout := defaultAskTimeout
	if req.TimeoutSecs > 0 {
		timeout = time.Duration(req.TimeoutSecs * float64(time.Second))
	}

	res := pending.Wait(ctx, timeout)
	switch res.Result {
	case approval.ResultApproved, approval.ResultDenied:
		return res.Reason, nil
	case approval.ResultTimeout:
		return "", errors.New("no answer within " + timeout.String())
	default:
		reason := res.Reason
		if reason == "" {
			reason = "cancelled"
		}
		return "", errors.New("ask_user " + reason)
	}
}

var _ meta.AskUserBridge = (*askUserBridge)(nil)
