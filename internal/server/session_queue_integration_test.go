package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// End-to-end through the REAL path: SubmitUserTurn → queue → loop dequeue →
// qt.persist (user_message) + onDequeued (message_started). Proves the row
// actually leaves the session's projection when its turn starts — the "message
// stays in the queue" bug. Uses a real bus and the runner's hooks wired exactly
// as bootstrap wires them.
func TestQueueIntegration_DequeueDrainsTheRow(t *testing.T) {
	d, done := newQueueTestDaemon(t)
	defer done()

	release := make(chan struct{})
	firstRunning := make(chan struct{})
	var once sync.Once
	runner := newSessionRunner(func(_ context.Context, in runtime.TurnInput) error {
		if in.ClientMessageID == "first" {
			once.Do(func() { close(firstRunning) })
			<-release
		}
		return nil
	}, time.Minute, d.logger)
	// Wire the hooks exactly like bootstrap.
	runner.queuedHook = d.onTurnQueued
	runner.dequeuedHook = d.onTurnDequeued
	d.sessionRunner = runner

	const sid = "s1"
	appendUser := func(cid, msg string) func() (uint64, error) {
		return func() (uint64, error) {
			return d.sessionStore.AppendDurable(context.Background(), sessionstore.Event{
				Type:      sessionstore.EventUserMessage,
				SessionID: sid,
				AppID:     "app",
				Message: &sessionstore.MessagePayload{
					Role: "user", Content: msg, ClientMessageID: cid,
				},
			})
		}
	}

	// First turn: idle → runs, holds the lane.
	_, _, _, err := runner.SubmitUserTurn(
		runtime.TurnInput{AppID: "app", SessionID: sid, ClientMessageID: "first", Message: "un"},
		appendUser("first", "un"))
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	<-firstRunning

	// Second message queues (deferred persist).
	queued, _, _, err := runner.SubmitUserTurn(
		runtime.TurnInput{AppID: "app", SessionID: sid, ClientMessageID: "second", Message: "deux"},
		appendUser("second", "deux"))
	if err != nil || !queued {
		t.Fatalf("second submit: queued=%v err=%v", queued, err)
	}

	// The row is in the queue projection right now.
	waitQueueLen(t, d, sid, 1, "second is queued")

	// Let the first turn finish: the loop dequeues #2, persists its user_message,
	// and fires onDequeued → message_started → the row must leave the queue.
	close(release)
	waitQueueLen(t, d, sid, 0, "second left the queue at dequeue")
}

func waitQueueLen(t *testing.T, d *Daemon, sid string, want int, what string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		st, err := d.sessionStore.State(sid)
		if err == nil && st != nil {
			st.RLock()
			n := len(st.Queue)
			st.RUnlock()
			if n == want {
				return
			}
		}
		select {
		case <-deadline:
			st, _ := d.sessionStore.State(sid)
			st.RLock()
			q := append([]sessionstore.QueueEntry(nil), st.Queue...)
			st.RUnlock()
			t.Fatalf("%s: queue len != %d, got %+v", what, want, q)
		case <-time.After(5 * time.Millisecond):
		}
	}
}
