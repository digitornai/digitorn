package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/background"
	"github.com/mbathepaul/digitorn/internal/runtime/context/meta"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// TestE2E_BackgroundRealtimeAndProactiveWake proves the whole background_run
// loop end-to-end through the real bus + Socket.IO bridge :
//
//   - launching a task pushes a "running" lifecycle event to the session's
//     realtime room, carrying the task payload (task_id, tool, state) ;
//   - completion pushes a "completed" event ;
//   - completion also proactively wakes the agent (a turn runs for that
//     session with no user message).
func TestE2E_BackgroundRealtimeAndProactiveWake(t *testing.T) {
	h := newAPIHarness(t)

	// The runner stands in for the engine ; it records proactive turns.
	var woke atomic.Int32
	var wokeSession atomic.Value
	runner := newSessionRunner(func(_ context.Context, in runtime.TurnInput) error {
		wokeSession.Store(in.SessionID)
		woke.Add(1)
		return nil
	}, time.Minute, testLogger())

	mgr := background.New()
	mgr.AttachDispatcher(bgInstantDispatcher{})
	mgr.AttachSink(h.bus)   // real-time client view via the bus → bridge
	mgr.AttachWaker(runner) // proactive wake

	// A real session so the events have a room to route to.
	code, body := h.do(t, "POST", "/api/apps/app-1/sessions", "user-A", `{"title":"t"}`)
	if code != 201 {
		t.Fatalf("create session: %d %s", code, body)
	}
	var created map[string]any
	decodeBody(t, body, &created)
	sid := created["session_id"].(string)

	// Launch a task (as the runtime would, with the real session id).
	tid, err := mgr.Launch(context.Background(), meta.LaunchRequest{
		SessionID: sid, AppID: "app-1", UserID: "user-A", Tool: "database.sql",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// The bridge must push both lifecycle events to session:<sid>, with the
	// task payload attached.
	room := "session:" + sid
	var sawRunning, sawCompleted bool
	waitUntil(t, func() bool {
		sawRunning, sawCompleted = false, false
		for _, e := range h.rt.recordedEmits() {
			if e.Room != room || e.Event != "event" {
				continue
			}
			env, ok := e.Data.(sessionstore.SocketEnvelope)
			if !ok || env.Type != string(sessionstore.EventBackgroundTask) {
				continue
			}
			p, ok := env.Payload.(*sessionstore.BackgroundTaskPayload)
			if !ok || p.TaskID != tid {
				continue
			}
			switch p.State {
			case "running":
				sawRunning = true
			case "completed":
				sawCompleted = true
			}
		}
		return sawRunning && sawCompleted
	}, "running + completed pushed to session room with payload")

	// Completion proactively woke the agent for THIS session.
	waitUntil(t, func() bool { return woke.Load() >= 1 }, "proactive wake fired")
	if got, _ := wokeSession.Load().(string); got != sid {
		t.Errorf("proactive turn ran for %q, want %q", got, sid)
	}
}
