package server

import (
	"context"

	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
	"github.com/google/uuid"
)

// Durable mirror of the session runner's in-memory FIFO.
//
// The runner owns SCHEDULING (see session_runner.go: append and drain are
// atomic against `running`). These hooks own DURABILITY and NOTIFICATION: they
// append the row to the session's event log so it survives a daemon restart,
// and tell that one session's room so the web panel updates live.
//
// Session scoping is the point: the event is appended to `in.SessionID` and
// emitted to `session:<in.SessionID>`. Nothing here broadcasts.

// onTurnQueued records a user message that arrived while a turn was running.
func (d *Daemon) onTurnQueued(in runtime.TurnInput, depth int) {
	if d == nil || in.SessionID == "" {
		return
	}
	// The correlation id ties the row to the turn it will become, so the web
	// can reconcile its optimistic entry and later drop it on message_started.
	cid := in.ClientMessageID
	if cid == "" {
		cid = uuid.NewString()
	}
	payload := &sessionstore.QueuePayload{
		ID:            uuid.NewString(),
		CorrelationID: cid,
		Message:       in.Message,
		Status:        "queued",
		Position:      depth,
	}

	ctx := context.Background()
	if d.sessionStore != nil {
		if _, err := d.sessionStore.AppendDurable(ctx, sessionstore.Event{
			Type:      sessionstore.EventMessageQueued,
			SessionID: in.SessionID,
			AppID:     in.AppID,
			UserID:    in.UserID,
			// Correlation on the ENVELOPE too: the projection matches
			// message_started / message_done on it to settle the row.
			CorrelationID: cid,
			Queue:         payload,
		}); err != nil && d.logger != nil {
			d.logger.Warn("queue: durable append failed",
				"session_id", in.SessionID, "err", err.Error())
		}
	}
	d.emitQueueEvent(ctx, in.SessionID, "message_queued", map[string]any{
		"session_id":     in.SessionID,
		"id":             payload.ID,
		"correlation_id": cid,
		"message":        in.Message,
		"status":         "queued",
		"position":       depth,
	})
}

// onTurnDequeued fires the instant a queued row leaves the FIFO to become the
// running turn. The projection settles the row on the durable message_started
// the engine already emits; this is the live signal for the panel.
func (d *Daemon) onTurnDequeued(in runtime.TurnInput) {
	if d == nil || in.SessionID == "" || in.ClientMessageID == "" {
		return
	}
	d.emitQueueEvent(context.Background(), in.SessionID, "message_started", map[string]any{
		"session_id":     in.SessionID,
		"correlation_id": in.ClientMessageID,
		"status":         "running",
	})
}

// emitQueueEvent sends to ONE session's room. Kept as the single exit point so
// a future queue event cannot accidentally be written as a broadcast.
func (d *Daemon) emitQueueEvent(ctx context.Context, sessionID, name string, payload map[string]any) {
	if d.rt == nil || sessionID == "" {
		return
	}
	_ = d.rt.Emit(ctx, bridgeNamespace, "session:"+sessionID, name, payload)
}
