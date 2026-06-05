package runtime

import (
	"context"
)

// BackgroundNotifier returns and clears the queue of background-task
// completion messages for a given session. The runtime calls it at
// turn_start and injects each entry as a synthetic user message in
// the doc-defined format per 04c-primitives.md "Auto-notification" :
//
//	[BACKGROUND TASK COMPLETED] task_id=a1b2c3d4 tool=database.sql elapsed=12.3s
//
// The agent does not need to poll — completed tasks surface
// automatically. The notification carries enough info for the agent
// to call background_run(task_id=...) again to fetch the full result.
type BackgroundNotifier interface {
	DrainNotifications(sessionID string) []BackgroundNotification
}

// BackgroundNotification is what the notifier returns for one
// completed task. The Message() method renders the doc-defined
// format.
type BackgroundNotification interface {
	Message() string
}

// injectBackgroundNotifications drains pending background-task
// completion messages for the given session and persists each through
// the unified system-directive path (durable EventSystemMessage,
// Role="system"). Called at the top of every turn, before the LLM
// round ; the turn re-snapshots afterwards so they land in context.
//
// Per docs-site/language/04c-primitives.md the runtime injects these as
// SYSTEM messages (not user) — they're authoritative status the agent
// must heed, not user input.
//
// Failures (notifier nil, append errors) degrade gracefully : nothing's
// injected, the turn continues. Notifications are NOT retried on the
// next turn — the doc contract is "fire once".
func (e *Engine) injectBackgroundNotifications(ctx context.Context, in TurnInput, turnID string) {
	if e == nil || e.BackgroundNotifications == nil {
		return
	}
	pending := e.BackgroundNotifications.DrainNotifications(in.SessionID)
	if len(pending) == 0 {
		return
	}
	for _, n := range pending {
		msg := n.Message()
		if msg == "" {
			continue
		}
		e.injectSystemDirective(ctx, in, turnID, msg, DirectiveBackgroundNotify, nil, nil)
	}
}
