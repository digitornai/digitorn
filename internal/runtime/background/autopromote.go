package background

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/context/meta"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

const DefaultPromoteThreshold = 2 * time.Minute

var promotableTools = map[string]bool{"bash.run": true}

const (
	stateRunning   = "running"
	stateCompleted = "completed"
	stateErrored   = "errored"
	stateCancelled = "cancelled"
)

type PromotingDispatcher struct {
	inner     runtime.ToolDispatcher
	mgr       *Manager
	threshold time.Duration
}

func NewPromotingDispatcher(inner runtime.ToolDispatcher, mgr *Manager, threshold time.Duration) *PromotingDispatcher {
	if threshold <= 0 {
		threshold = DefaultPromoteThreshold
	}
	return &PromotingDispatcher{inner: inner, mgr: mgr, threshold: threshold}
}

func (p *PromotingDispatcher) Dispatch(ctx context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	name := meta.ResolveAlias(meta.Canonicalize(call.Name))
	if !p.eligible(ctx, name) {
		return p.inner.Dispatch(ctx, call)
	}
	taskID, err := p.mgr.Launch(ctx, meta.LaunchRequest{
		SessionID: call.SessionID,
		AppID:     call.AppID,
		UserID:    call.UserID,
		AgentID:   call.AgentID,
		Tool:      name,
		Args:      call.Args,
	})
	if err != nil {
		return p.inner.Dispatch(context.WithoutCancel(ctx), call)
	}
	st, werr := p.mgr.Wait(ctx, call.SessionID, taskID, p.threshold.Seconds())
	switch {
	case werr == nil:
		return outcomeFromStatus(st)
	case errors.Is(werr, context.DeadlineExceeded) && st.State == stateRunning:
		return p.promotedOutcome(taskID)
	default:
		if st.State == stateRunning {
			return p.promotedOutcome(taskID)
		}
		return outcomeFromStatus(st)
	}
}

func (p *PromotingDispatcher) eligible(ctx context.Context, canonicalName string) bool {
	if p.mgr == nil || p.inner == nil {
		return false
	}
	if tool.IsBackground(ctx) {
		return false
	}
	return promotableTools[canonicalName]
}

func outcomeFromStatus(st meta.BackgroundStatus) runtime.ToolOutcome {
	status := stateCompleted
	if st.State == stateErrored || st.State == stateCancelled {
		status = stateErrored
	}
	text, _ := st.Result.(string)
	if text == "" {
		text = st.Log
	}
	return runtime.ToolOutcome{
		Status: status,
		Error:  st.Error,
		Parts:  []sessionstore.MessagePart{{Type: "text", Text: text}},
	}
}

func (p *PromotingDispatcher) promotedOutcome(taskID string) runtime.ToolOutcome {
	mins := int(p.threshold.Round(time.Minute).Minutes())
	if mins < 1 {
		mins = 1
	}
	msg := fmt.Sprintf(
		"The command is still running after %dm, so it was moved to the background as task_id=%q. It is NOT killed — it keeps running, and you will be NOTIFIED automatically when it finishes (success or failure).\n"+
			"Don't wait idly:\n"+
			"  - check its progress / captured logs anytime: background_run(task_id=%q)\n"+
			"  - cancel it if needed: background_run(task_id=%q, cancel:true)\n"+
			"Get on with other work meanwhile; the result will arrive as a system notification.",
		mins, taskID, taskID, taskID)
	return runtime.ToolOutcome{
		Status:   stateCompleted,
		Parts:    []sessionstore.MessagePart{{Type: "text", Text: msg}},
		Metadata: map[string]any{"promoted": true, "promoted_task_id": taskID},
	}
}

func (p *PromotingDispatcher) CallAppWired() bool {
	if pa, ok := p.inner.(interface{ CallAppWired() bool }); ok {
		return pa.CallAppWired()
	}
	return false
}

func (p *PromotingDispatcher) AskUserWired() bool {
	if pa, ok := p.inner.(interface{ AskUserWired() bool }); ok {
		return pa.AskUserWired()
	}
	return false
}

func (p *PromotingDispatcher) UseSkillWired() bool {
	if pa, ok := p.inner.(interface{ UseSkillWired() bool }); ok {
		return pa.UseSkillWired()
	}
	return false
}

func (p *PromotingDispatcher) ResolveToolName(appID, agentID, name string) string {
	if r, ok := p.inner.(interface {
		ResolveToolName(appID, agentID, name string) string
	}); ok {
		return r.ResolveToolName(appID, agentID, name)
	}
	return name
}

func (p *PromotingDispatcher) IndexFQNs(appID, agentID string) []string {
	if r, ok := p.inner.(interface {
		IndexFQNs(appID, agentID string) []string
	}); ok {
		return r.IndexFQNs(appID, agentID)
	}
	return nil
}
