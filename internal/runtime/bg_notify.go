package runtime

import (
	"context"
)

type BackgroundNotifier interface {
	DrainNotifications(sessionID string) []BackgroundNotification
}

type BackgroundNotification interface {
	Message() string
}

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
