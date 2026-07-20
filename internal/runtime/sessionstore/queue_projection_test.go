package sessionstore_test

import (
	"testing"

	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// The queue is DURABLE: it is a projection of events, so a daemon restart
// rebuilds it from disk. A memory-only queue would silently drop the messages
// the user typed while a turn was running.

func apply(s *sessionstore.SessionState, evs ...sessionstore.Event) {
	for i := range evs {
		if evs[i].TsUnixNano == 0 {
			evs[i].TsUnixNano = int64(i + 1)
		}
		sessionstore.Apply(s, &evs[i])
	}
}

func queued(id, cid, msg string) sessionstore.Event {
	return sessionstore.Event{
		Type:  sessionstore.EventMessageQueued,
		Queue: &sessionstore.QueuePayload{ID: id, CorrelationID: cid, Message: msg},
	}
}

func TestQueue_FIFOAndPositions(t *testing.T) {
	s := sessionstore.NewSessionState("s1")
	apply(s, queued("q1", "c1", "un"), queued("q2", "c2", "deux"), queued("q3", "c3", "trois"))

	if len(s.Queue) != 3 {
		t.Fatalf("len = %d, want 3", len(s.Queue))
	}
	for i, want := range []struct {
		msg string
		pos int
	}{{"un", 1}, {"deux", 2}, {"trois", 3}} {
		if s.Queue[i].Message != want.msg || s.Queue[i].Position != want.pos {
			t.Errorf("row %d = (%q, pos %d), want (%q, pos %d)",
				i, s.Queue[i].Message, s.Queue[i].Position, want.msg, want.pos)
		}
		if s.Queue[i].Status != "queued" {
			t.Errorf("row %d status = %q, want queued", i, s.Queue[i].Status)
		}
	}
}

func TestQueue_StartedThenDoneLeavesTheQueue(t *testing.T) {
	s := sessionstore.NewSessionState("s1")
	apply(s, queued("q1", "c1", "un"), queued("q2", "c2", "deux"))

	apply(s, sessionstore.Event{Type: sessionstore.EventMessageStarted, CorrelationID: "c1"})
	if s.Queue[0].Status != "running" || s.Queue[0].Position != 0 {
		t.Fatalf("running row = (%q, pos %d), want (running, pos 0)", s.Queue[0].Status, s.Queue[0].Position)
	}
	// The one still pending must be renumbered to 1, not left at 2.
	if s.Queue[1].Position != 1 {
		t.Errorf("pending row position = %d, want 1", s.Queue[1].Position)
	}

	apply(s, sessionstore.Event{Type: sessionstore.EventMessageDone, CorrelationID: "c1"})
	if len(s.Queue) != 1 || s.Queue[0].CorrelationID != "c2" {
		t.Fatalf("after done: %+v", s.Queue)
	}
	if s.Queue[0].Position != 1 {
		t.Errorf("remaining row position = %d, want 1", s.Queue[0].Position)
	}
}

func TestQueue_CancelByIDAndByCorrelation(t *testing.T) {
	s := sessionstore.NewSessionState("s1")
	apply(s, queued("q1", "c1", "un"), queued("q2", "c2", "deux"), queued("q3", "c3", "trois"))

	apply(s, sessionstore.Event{
		Type:  sessionstore.EventMessageCancelled,
		Queue: &sessionstore.QueuePayload{ID: "q2"},
	})
	if len(s.Queue) != 2 || s.Queue[1].ID != "q3" {
		t.Fatalf("cancel by id: %+v", s.Queue)
	}

	apply(s, sessionstore.Event{Type: sessionstore.EventMessageCancelled, CorrelationID: "c3"})
	if len(s.Queue) != 1 || s.Queue[0].ID != "q1" {
		t.Fatalf("cancel by correlation: %+v", s.Queue)
	}
	if s.Queue[0].Position != 1 {
		t.Errorf("position after cancels = %d, want 1", s.Queue[0].Position)
	}
}

// Clearing drops what is WAITING, never the turn already in flight.
func TestQueue_ClearKeepsTheRunningRow(t *testing.T) {
	s := sessionstore.NewSessionState("s1")
	apply(s, queued("q1", "c1", "un"), queued("q2", "c2", "deux"), queued("q3", "c3", "trois"))
	apply(s, sessionstore.Event{Type: sessionstore.EventMessageStarted, CorrelationID: "c1"})

	apply(s, sessionstore.Event{Type: sessionstore.EventQueueCleared})

	if len(s.Queue) != 1 {
		t.Fatalf("after clear: %+v", s.Queue)
	}
	if s.Queue[0].CorrelationID != "c1" || s.Queue[0].Status != "running" {
		t.Errorf("clear removed the running row: %+v", s.Queue[0])
	}
}

// Replay must be idempotent: the same event applied twice (reconnect backfill,
// restart) must not duplicate the row.
func TestQueue_ReplayIsIdempotent(t *testing.T) {
	s := sessionstore.NewSessionState("s1")
	e := queued("q1", "c1", "un")
	apply(s, e, e, e)

	if len(s.Queue) != 1 {
		t.Fatalf("replay duplicated the row: %+v", s.Queue)
	}
}

// A cold rebuild from the event log must reproduce the live projection — this
// is what "durable" actually means.
func TestQueue_ColdRebuildMatchesLiveState(t *testing.T) {
	events := []sessionstore.Event{
		queued("q1", "c1", "un"),
		queued("q2", "c2", "deux"),
		{Type: sessionstore.EventMessageStarted, CorrelationID: "c1"},
		queued("q3", "c3", "trois"),
		{Type: sessionstore.EventMessageDone, CorrelationID: "c1"},
	}

	live := sessionstore.NewSessionState("s1")
	apply(live, events...)

	cold := sessionstore.NewSessionState("s1")
	apply(cold, events...)

	if len(cold.Queue) != len(live.Queue) {
		t.Fatalf("cold len %d != live len %d", len(cold.Queue), len(live.Queue))
	}
	for i := range live.Queue {
		if cold.Queue[i] != live.Queue[i] {
			t.Errorf("row %d differs:\n cold %+v\n live %+v", i, cold.Queue[i], live.Queue[i])
		}
	}
	if len(live.Queue) != 2 || live.Queue[0].Position != 1 || live.Queue[1].Position != 2 {
		t.Errorf("unexpected final queue: %+v", live.Queue)
	}
}

// A queued row carries no message body in the transcript, so an empty id must
// never create a phantom row.
func TestQueue_IgnoresMalformedEvents(t *testing.T) {
	s := sessionstore.NewSessionState("s1")
	apply(s,
		sessionstore.Event{Type: sessionstore.EventMessageQueued},
		sessionstore.Event{Type: sessionstore.EventMessageQueued, Queue: &sessionstore.QueuePayload{}},
		sessionstore.Event{Type: sessionstore.EventMessageCancelled},
	)
	if len(s.Queue) != 0 {
		t.Fatalf("malformed events created rows: %+v", s.Queue)
	}
}
