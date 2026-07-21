package server

import (
	"context"

	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
	"github.com/google/uuid"
)

// Durable mirror of the session runner's in-memory FIFO.
//
// The runner owns SCHEDULING (session_runner.go: append and drain are atomic
// against `running`). These hooks own DURABILITY and NOTIFICATION.
//
// BOTH go through AppendDurable, never a bare rt.Emit. Only durable events
// reach the web: the bridge (socketio_bridge.go) subscribes to the session
// bus and fans every appended event to `session:<id>` as an "event" envelope.
// A named emit ("message_started") is NOT on that path and the client — which
// listens solely on socket.on("event") — never receives it. Going through the
// bus also makes isolation structural: the bridge routes by the event's
// SessionID, so a queue event can only ever reach its own session's room.
//
// Persisting these events is deliberate: the queue is durable, so a cold
// rebuild must reproduce it. message_queued adds the row; message_started
// removes it (the message left the queue to become the running turn). Without
// the durable message_started, a restart mid-turn would resurrect a row that
// is already in the chat.

// onTurnQueued records a user message that arrived while a turn was running.
func (d *Daemon) onTurnQueued(in runtime.TurnInput, depth int) {
	if d == nil || d.sessionStore == nil || in.SessionID == "" {
		return
	}
	cid := in.ClientMessageID
	if cid == "" {
		cid = uuid.NewString()
	}
	ev := sessionstore.Event{
		Type:      sessionstore.EventMessageQueued,
		SessionID: in.SessionID,
		AppID:     in.AppID,
		UserID:    in.UserID,
		// Correlation on the envelope: the projection and the web match the row
		// on it across message_queued / message_started.
		CorrelationID: cid,
		Queue: &sessionstore.QueuePayload{
			ID:            uuid.NewString(),
			CorrelationID: cid,
			Message:       in.Message,
			Status:        "queued",
			Position:      depth,
		},
	}
	if _, err := d.sessionStore.AppendDurable(context.Background(), ev); err != nil && d.logger != nil {
		d.logger.Warn("queue: message_queued append failed",
			"session_id", in.SessionID, "err", err.Error())
	}
}

// onTurnDequeued fires when a queued row leaves the FIFO to become the running
// turn. The durable message_started removes the row from the queue projection
// and, via the bridge, tells the web to drop it from the panel — the message
// is now a chat bubble (its user_message was just persisted by the loop).
func (d *Daemon) onTurnDequeued(in runtime.TurnInput) {
	if d == nil || d.sessionStore == nil || in.SessionID == "" || in.ClientMessageID == "" {
		return
	}
	ev := sessionstore.Event{
		Type:          sessionstore.EventMessageStarted,
		SessionID:     in.SessionID,
		AppID:         in.AppID,
		UserID:        in.UserID,
		CorrelationID: in.ClientMessageID,
	}
	if _, err := d.sessionStore.AppendDurable(context.Background(), ev); err != nil && d.logger != nil {
		d.logger.Warn("queue: message_started append failed",
			"session_id", in.SessionID, "err", err.Error())
	}
}
