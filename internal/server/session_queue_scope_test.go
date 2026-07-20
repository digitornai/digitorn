package server

import (
	"context"
	"sync"
	"testing"

	"github.com/digitornai/digitorn/internal/ports"
	"github.com/digitornai/digitorn/internal/runtime"
)

// A queued row belongs to ONE session: the durable append targets that session
// and the realtime event goes to that session's room. Never a broadcast.
//
// These assert the property through the emit surface rather than by reading the
// code — a future refactor that reintroduces a broadcast fails here.

type capturedEmit struct {
	room    string
	name    string
	payload map[string]any
}

type recordingRT struct {
	mu         sync.Mutex
	sent       []capturedEmit
	broadcasts int
}

func (r *recordingRT) Emit(_ context.Context, _ string, room, name string, payload any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, _ := payload.(map[string]any)
	r.sent = append(r.sent, capturedEmit{room: room, name: name, payload: m})
	return nil
}

// Broadcast reaches EVERY connected client. A queue event must never take this
// path; the counter turns that into a test failure instead of a silent leak.
func (r *recordingRT) Broadcast(_ context.Context, _, _ string, _ any) error {
	r.mu.Lock()
	r.broadcasts++
	r.mu.Unlock()
	return nil
}

func (r *recordingRT) SetAuthHandler(ports.AuthHandler)          {}
func (r *recordingRT) OnConnection(ports.ConnectionHandler)      {}
func (r *recordingRT) OnDisconnection(ports.DisconnectHandler)   {}
func (r *recordingRT) OnEvent(string, ports.EventHandler)        {}
func (r *recordingRT) Close(context.Context) error               { return nil }

func (r *recordingRT) broadcastCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.broadcasts
}

func (r *recordingRT) all() []capturedEmit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]capturedEmit(nil), r.sent...)
}

func TestQueueEvent_GoesOnlyToItsOwnSessionRoom(t *testing.T) {
	rt := &recordingRT{}
	d := &Daemon{rt: rt}

	d.onTurnQueued(runtime.TurnInput{
		AppID: "app", SessionID: "session-a", UserID: "u",
		ClientMessageID: "cid-a", Message: "bonjour",
	}, 1)
	d.onTurnQueued(runtime.TurnInput{
		AppID: "app", SessionID: "session-b", UserID: "u",
		ClientMessageID: "cid-b", Message: "salut",
	}, 1)

	sent := rt.all()
	if len(sent) != 2 {
		t.Fatalf("emitted %d events, want 2", len(sent))
	}
	for _, e := range sent {
		switch e.payload["correlation_id"] {
		case "cid-a":
			if e.room != "session:session-a" {
				t.Errorf("session A event went to room %q", e.room)
			}
			if e.payload["session_id"] != "session-a" {
				t.Errorf("session A payload carries session_id %v", e.payload["session_id"])
			}
		case "cid-b":
			if e.room != "session:session-b" {
				t.Errorf("session B event went to room %q", e.room)
			}
		default:
			t.Errorf("unexpected correlation %v", e.payload["correlation_id"])
		}
		// A room-less emit would reach every connected client.
		if e.room == "" {
			t.Error("queue event emitted without a room (broadcast)")
		}
	}
	if n := rt.broadcastCount(); n != 0 {
		t.Errorf("%d queue events went out as broadcasts — every session saw them", n)
	}
}

func TestQueueEvent_CarriesTheContractTheWebParses(t *testing.T) {
	rt := &recordingRT{}
	d := &Daemon{rt: rt}

	d.onTurnQueued(runtime.TurnInput{
		AppID: "app", SessionID: "s1", UserID: "u",
		ClientMessageID: "cid-1", Message: "construis un dashboard",
	}, 3)

	sent := rt.all()
	if len(sent) != 1 || sent[0].name != "message_queued" {
		t.Fatalf("unexpected emits: %+v", sent)
	}
	p := sent[0].payload
	// Keys the web's queueEntryFromJson reads. A rename here silently empties
	// the queue panel, so pin them.
	for _, k := range []string{"id", "correlation_id", "message", "status", "position", "session_id"} {
		if _, ok := p[k]; !ok {
			t.Errorf("payload missing %q: %+v", k, p)
		}
	}
	if p["status"] != "queued" {
		t.Errorf("status = %v, want queued", p["status"])
	}
	if p["position"] != 3 {
		t.Errorf("position = %v, want 3 (queue depth)", p["position"])
	}
	if p["message"] != "construis un dashboard" {
		t.Errorf("message = %v", p["message"])
	}
	if p["id"] == p["correlation_id"] {
		t.Error("row id must be distinct from the correlation id")
	}
}

// The dequeue signal must name the row it promotes, or the panel cannot clear it.
func TestQueueEvent_DequeueNamesItsRow(t *testing.T) {
	rt := &recordingRT{}
	d := &Daemon{rt: rt}

	d.onTurnDequeued(runtime.TurnInput{
		AppID: "app", SessionID: "s1", ClientMessageID: "cid-1",
	})

	sent := rt.all()
	if len(sent) != 1 {
		t.Fatalf("emitted %d events, want 1", len(sent))
	}
	if sent[0].name != "message_started" || sent[0].room != "session:s1" {
		t.Fatalf("unexpected emit: %+v", sent[0])
	}
	if sent[0].payload["correlation_id"] != "cid-1" {
		t.Errorf("correlation = %v, want cid-1", sent[0].payload["correlation_id"])
	}
}

// A proactive wake carries no client message id: it is not a user message and
// must not produce a phantom queue row.
func TestQueueEvent_NoRowForAProactiveWake(t *testing.T) {
	rt := &recordingRT{}
	d := &Daemon{rt: rt}

	d.onTurnDequeued(runtime.TurnInput{AppID: "app", SessionID: "s1"})

	if n := len(rt.all()); n != 0 {
		t.Errorf("emitted %d events for a wake with no correlation, want 0", n)
	}
}
