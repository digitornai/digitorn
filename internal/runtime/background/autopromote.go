package background

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/context/meta"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// DefaultPromoteThreshold is how long a foreground command may block a turn
// before it is auto-promoted to a managed background task.
const DefaultPromoteThreshold = 2 * time.Minute

// promotableTools is the canonical set of tool names eligible for auto-promotion.
// Only bash `run` qualifies — it is the one foreground tool that legitimately
// runs for minutes (builds, installs, test suites); read/edit/search are sub-second.
var promotableTools = map[string]bool{"bash.run": true}

const (
	stateRunning   = "running"
	stateCompleted = "completed"
	stateErrored   = "errored"
	stateCancelled = "cancelled"
)

// PromotingDispatcher wraps a ToolDispatcher so a FOREGROUND `bash.run` still
// running after the threshold is auto-promoted to a managed background task
// instead of blocking the turn or being killed at a timeout. The agent is never
// blocked longer than the threshold, the command keeps running, and the existing
// background pipeline — status checks via background_run, the turn-start
// completion notification, the proactive wake — takes over. Every other call
// passes straight through to the inner dispatcher.
//
// It sits AFTER the engine's approval/gate chokepoint (it IS the engine's
// Dispatcher, set once the gate has run), so it only ever promotes a call the
// user already approved — the background launch needs no second gate, exactly
// like background_run.
type PromotingDispatcher struct {
	inner     runtime.ToolDispatcher
	mgr       *Manager
	threshold time.Duration
}

// NewPromotingDispatcher wraps inner. threshold<=0 uses DefaultPromoteThreshold.
// A nil mgr disables promotion (everything passes through) — safe for tests/dev.
func NewPromotingDispatcher(inner runtime.ToolDispatcher, mgr *Manager, threshold time.Duration) *PromotingDispatcher {
	if threshold <= 0 {
		threshold = DefaultPromoteThreshold
	}
	return &PromotingDispatcher{inner: inner, mgr: mgr, threshold: threshold}
}

// Dispatch runs a promotable foreground bash run via the background manager,
// returning its result if it finishes within the threshold (transparent to the
// agent) or a "moved to background" handoff if it is still running.
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
		// Per-session task cap reached, or no dispatcher — run it the ordinary way.
		return p.inner.Dispatch(ctx, call)
	}
	// The Wait settle-window suppresses the duplicate completion notification: if
	// the task finishes here, the agent gets the result directly; if it times out,
	// the waiter deregisters and the task notifies on completion later.
	st, werr := p.mgr.Wait(ctx, call.SessionID, taskID, p.threshold.Seconds())
	switch {
	case werr == nil:
		return outcomeFromStatus(st) // finished within the threshold — transparent
	case errors.Is(werr, context.DeadlineExceeded) && st.State == stateRunning:
		return p.promotedOutcome(taskID) // still running — hand off to background
	default:
		// Parent ctx cancelled (turn abort), or a terminal state raced the
		// deadline: return whatever the snapshot holds.
		return outcomeFromStatus(st)
	}
}

func (p *PromotingDispatcher) eligible(ctx context.Context, canonicalName string) bool {
	if p.mgr == nil || p.inner == nil {
		return false
	}
	if tool.IsBackground(ctx) {
		return false // an explicit background_run is already managed
	}
	return promotableTools[canonicalName]
}

// outcomeFromStatus rebuilds the foreground-equivalent tool outcome from a
// finished task's snapshot. A bash run's result is its captured text output (with
// the cwd / elapsed / git enrichment the module already folds in), so the agent
// sees exactly what a direct foreground run would have returned.
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

// promotedOutcome is the synchronous handoff returned to the agent the moment a
// foreground command crosses the threshold. It is a SUCCESS (the command is fine,
// just long) and routes the agent to the existing background tooling.
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
