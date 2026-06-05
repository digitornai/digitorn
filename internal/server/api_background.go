package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// listBackgroundTasks returns every background_run task for one session.
// The client uses it to render the live task list ; real-time updates flow
// separately via the background_task events on the Socket.IO bridge. The
// session must be owned by the caller.
func (d *Daemon) listBackgroundTasks(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	state, err := d.requireOwnedSession(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	// tasks : the DURABLE, projected task list rebuilt from background_task
	// events — it survives a daemon restart and cold-load, so a reconnecting
	// client reconstructs the full list (with running tasks reconciled to
	// interrupted) even when the in-memory registry is empty.
	state.RLock()
	durable := append([]sessionstore.BackgroundTaskState(nil), state.BackgroundTasks...)
	state.RUnlock()
	resp := map[string]any{"session_id": sid, "tasks": durable, "count": len(durable)}
	// live : the in-memory registry view (this daemon instance) — real-time
	// status while the daemon is up. Empty after a restart.
	if d.background != nil {
		if live, lerr := d.background.List(r.Context(), sid); lerr == nil {
			resp["live"] = live
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// cancelBackgroundTask signals a running background task to stop. Returns
// immediately ; the task transitions to "cancelled" when its goroutine
// observes the cancel, which emits the durable + realtime "cancelled"
// lifecycle event. Idempotent from the client's view : cancelling an
// already-finished task is a no-op signal. The session must be owned by
// the caller, so one user can't cancel another's tasks.
func (d *Daemon) cancelBackgroundTask(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	taskID := chi.URLParam(r, "task_id")
	if _, err := d.requireOwnedSession(r.Context(), sid); err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	if d.background == nil {
		writeError(w, http.StatusServiceUnavailable, "background_unavailable", "background manager not running")
		return
	}
	if err := d.background.Cancel(r.Context(), sid, taskID); err != nil {
		writeError(w, http.StatusNotFound, "task_not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": sid, "task_id": taskID, "cancelled": true})
}
