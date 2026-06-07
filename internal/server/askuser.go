package server

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mbathepaul/digitorn/internal/runtime/context/meta"
	"github.com/mbathepaul/digitorn/internal/runtime/policy/approval"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// askUserBridge is the production AskUserBridge : it backs the
// context_builder.ask_user meta-tool with the same human-in-the-loop
// machinery as SG-5 approvals (doc-conform : "ask a question" and
// "approve an action" are the same synchronous-pause primitive with a
// different UI hint). It emits a durable EventApprovalRequest with
// kind="question" onto the session bus — the live client renders the
// question and the user answers via POST /api/apps/{id}/approve with
// {action:"approve", reason:"<their answer>"} — then blocks the turn
// goroutine on the approval registry until the answer (or a timeout)
// arrives.
type askUserBridge struct {
	store  *sessionstore.Bus
	reg    *approval.Registry
	logger *slog.Logger
}

// defaultAskTimeout matches the approval default (security-01-approval.md).
const defaultAskTimeout = 300 * time.Second

// Ask implements meta.AskUserBridge.
func (b *askUserBridge) Ask(ctx context.Context, req meta.AskUserRequest) (string, error) {
	if b == nil || b.store == nil || b.reg == nil {
		return "", errors.New("ask_user bridge not wired")
	}
	if req.SessionID == "" {
		return "", errors.New("ask_user: no session to address the question to")
	}

	id := uuid.NewString()
	// Carry the full interaction shape so the client renders the right
	// control (review box / buttons / dropdown / form) instead of a bare
	// text input. Empty fields are omitted to keep the payload lean.
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
	// The custom-answer escape hatch is carried whenever the agent makes proposals
	// (choices or a form) so the client always offers a "type your own" field —
	// unless the agent set allow_custom:false for a strict enum.
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
	// Arm the waiter BEFORE emitting the question event : a fast client can
	// answer before this reaches Wait, and arming first means Resolve always
	// finds the waiter (no emit-before-wait race, no lost answer).
	pending := b.reg.Arm(id)

	// Durable so the question survives compaction and a cold client
	// reconnect can still render the pending prompt.
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
		// The user's typed answer rides in Reason for both verbs : an
		// "approve" carries the answer, a "deny" may carry a refusal
		// note. Either way the agent gets the human's words.
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

// compile-time guard.
var _ meta.AskUserBridge = (*askUserBridge)(nil)
