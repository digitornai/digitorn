package server

import (
	"context"
	"net/http"

	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
	"github.com/go-chi/chi/v5"
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
			Message:         in.Message,
			Status:          "queued",
			Position:        depth,
			AttachmentCount: in.AttachmentCount,
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

// deleteQueueEntry cancels ONE waiting message (DELETE /queue/{entry_id}). The
// entry id the web sends is the message's client id (see queue.ts: the row's id
// becomes its correlation id after reconcile).
func (d *Daemon) deleteQueueEntry(w http.ResponseWriter, r *http.Request) {
	sid := sessionIDParam(r)
	if _, err := d.requireOwnedSession(r.Context(), sid); err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	entryID := chi.URLParam(r, "entry_id")
	appID := chi.URLParam(r, "app_id")
	uid := userIDOf(r.Context())

	removed := d.sessionRunner != nil && d.sessionRunner.CancelQueued(sid, entryID)
	if removed {
		d.emitQueueCancelled(sid, appID, uid, entryID)
	}
	// Idempotent: a row already gone (dequeued a moment ago, or a double click)
	// is a 200 with cancelled:false, not an error — the client's optimistic
	// removal already reflects the intent.
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sid,
		"entry_id":   entryID,
		"cancelled":  removed,
	})
}

// clearQueue drops every WAITING message (POST /queue/clear). The running turn
// is never in the queue, so it is untouched.
func (d *Daemon) clearQueue(w http.ResponseWriter, r *http.Request) {
	sid := sessionIDParam(r)
	if _, err := d.requireOwnedSession(r.Context(), sid); err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	appID := chi.URLParam(r, "app_id")
	uid := userIDOf(r.Context())

	var ids []string
	if d.sessionRunner != nil {
		ids = d.sessionRunner.ClearQueued(sid)
	}
	for _, cid := range ids {
		d.emitQueueCancelled(sid, appID, uid, cid)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sid,
		"cleared":    len(ids),
	})
}

// emitQueueCancelled removes a waiting row everywhere: durable so a cold rebuild
// agrees, and fanned to the session's room so the panel drops it live. Same
// SessionID-scoped path as the other queue events — never a broadcast.
func (d *Daemon) emitQueueCancelled(sid, appID, uid, clientMessageID string) {
	if d == nil || d.sessionStore == nil || sid == "" || clientMessageID == "" {
		return
	}
	ev := sessionstore.Event{
		Type:          sessionstore.EventMessageCancelled,
		SessionID:     sid,
		AppID:         appID,
		UserID:        uid,
		CorrelationID: clientMessageID,
	}
	if _, err := d.sessionStore.AppendDurable(context.Background(), ev); err != nil && d.logger != nil {
		d.logger.Warn("queue: message_cancelled append failed",
			"session_id", sid, "err", err.Error())
	}
}

// rehydrateQueue recovers queued messages that survived a daemon restart. The
// durable projection still has them, but the in-memory runner lost them, so
// they display "in queue" and never run. Runs ONCE per session (TryMarkRehydrated).
//
// Strategy: for each still-queued row, cancel the stale durable row, then
// re-submit it through the normal path (SubmitUserTurn), which runs it now if
// the lane is free or re-queues it — preserving FIFO order. A row with
// attachments cannot be rebuilt (only the COUNT was persisted, not the blobs),
// so it is cancelled without re-submit rather than sent stripped of its files.
func (d *Daemon) rehydrateQueue(sid, appID, userID, userJWT string) {
	if d == nil || d.sessionRunner == nil || d.sessionStore == nil {
		return
	}
	// One-shot: only the first opener recovers the queue.
	if !d.sessionRunner.TryMarkRehydrated(sid) {
		return
	}
	// A turn already running for this session means its queue is live in memory,
	// not orphaned — nothing to recover.
	if d.sessionRunner.IsRunning(sid) {
		return
	}
	state, err := d.sessionStore.State(sid)
	if err != nil || state == nil {
		return
	}
	state.RLock()
	orphans := make([]sessionstore.QueueEntry, 0, len(state.Queue))
	for _, e := range state.Queue {
		if e.Status == "queued" && e.CorrelationID != "" {
			orphans = append(orphans, e)
		}
	}
	state.RUnlock()
	if len(orphans) == 0 {
		return
	}

	for i := range orphans {
		e := orphans[i]
		// Drop the stale durable row first; the re-submit (if any) creates a
		// fresh one, and the client converges on the new state.
		d.emitQueueCancelled(sid, appID, userID, e.CorrelationID)

		if e.AttachmentCount > 0 {
			// Blobs are gone — only the count was persisted. Cancelling is the
			// honest outcome: better a missing message the user can resend than
			// one silently stripped of its images.
			if d.logger != nil {
				d.logger.Warn("queue: dropping orphaned message with attachments (blobs not recoverable)",
					"session_id", sid, "correlation_id", e.CorrelationID, "attachments", e.AttachmentCount)
			}
			continue
		}

		in := runtime.TurnInput{
			AppID:           appID,
			SessionID:       sid,
			UserID:          userID,
			UserJWT:         userJWT,
			ClientMessageID: e.CorrelationID,
			Message:         e.Message,
		}
		msg := e.Message
		cid := e.CorrelationID
		persist := func() (uint64, error) {
			ctxA, cancelA := appendCtx(context.Background())
			defer cancelA()
			return d.sessionStore.AppendDurable(ctxA, sessionstore.Event{
				Type:      sessionstore.EventUserMessage,
				SessionID: sid,
				AppID:     appID,
				UserID:    userID,
				Message: &sessionstore.MessagePayload{
					Role:            "user",
					Content:         msg,
					ClientMessageID: cid,
				},
			})
		}
		if _, _, _, serr := d.sessionRunner.SubmitUserTurn(in, persist); serr != nil && d.logger != nil {
			d.logger.Warn("queue: rehydrate re-submit failed",
				"session_id", sid, "correlation_id", cid, "err", serr.Error())
		}
	}
}
