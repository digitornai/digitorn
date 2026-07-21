package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// Detection: the turn-end safety net fires only while an undrained inject row
// is still waiting in the queue.
func TestInject_HasPendingDetection(t *testing.T) {
	cases := []struct {
		name  string
		queue []sessionstore.QueueEntry
		want  bool
	}{
		{"empty", nil, false},
		{"plain queued row (queue mode)", []sessionstore.QueueEntry{{ID: "a"}}, false},
		{"one inject row", []sessionstore.QueueEntry{{ID: "a", Inject: true}}, true},
		{"mixed", []sessionstore.QueueEntry{{ID: "a"}, {ID: "b", Inject: true}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasPendingInject(c.queue); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// End-to-end of the mechanic: an inject send enqueues a tagged row (queue panel,
// NOT the transcript); the drain then folds it into the transcript as a
// user_message + directive and removes the row. This is what makes the message
// wait in the queue until the injection window, then land with its seq.
func TestInject_EnqueueThenDrain(t *testing.T) {
	d, done := newQueueTestDaemon(t)
	defer done()

	if err := d.enqueueMidTurnInject("s1", "app", "u", "cid-1", "fais plutôt X", 0); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Before the drain: a row in the queue, nothing in the transcript.
	if q := queueOf(t, d, "s1"); len(q) != 1 || !q[0].Inject || q[0].Message != "fais plutôt X" {
		t.Fatalf("after enqueue: %+v, want one inject row", q)
	}
	state, _ := d.sessionStore.State("s1")
	state.RLock()
	nMsgs := len(state.Messages)
	state.RUnlock()
	if nMsgs != 0 {
		t.Fatalf("transcript has %d msgs before drain, want 0 (message waits in queue)", nMsgs)
	}

	// The drain (what the engine calls at a tool-call boundary).
	if !d.drainMidTurnInject(context.Background(), "s1", "app") {
		t.Fatal("drain reported nothing injected")
	}
	if q := queueOf(t, d, "s1"); len(q) != 0 {
		t.Fatalf("after drain: %+v, want empty (row folded in)", q)
	}
	state.RLock()
	defer state.RUnlock()
	if len(state.Messages) != 2 ||
		state.Messages[0].Role != "user" || state.Messages[0].Content != "fais plutôt X" ||
		state.Messages[1].Role != "system" || state.Messages[1].Content != midTurnInjectDirective {
		t.Fatalf("transcript after drain = %+v, want user bubble + directive", state.Messages)
	}
	// Idempotent: a second drain finds nothing (row already gone).
	if d.drainMidTurnInject(context.Background(), "s1", "app") {
		t.Error("second drain injected again — not idempotent")
	}
}

// injectMidTurn persists the user message (a bubble) then the steering directive.
func TestInject_PersistsUserThenDirective(t *testing.T) {
	d, done := newQueueTestDaemon(t)
	defer done()

	seq, err := d.injectMidTurn("s1", "app", "u", "cid-1", "arrête, fais plutôt X")
	if err != nil {
		t.Fatalf("injectMidTurn: %v", err)
	}
	if seq == 0 {
		t.Fatal("expected a durable seq for the user message")
	}
	state, _ := d.sessionStore.State("s1")
	state.RLock()
	defer state.RUnlock()
	if len(state.Messages) != 2 {
		t.Fatalf("got %d messages, want 2 (user + directive)", len(state.Messages))
	}
	if state.Messages[0].Role != "user" || state.Messages[0].Content != "arrête, fais plutôt X" {
		t.Errorf("msg[0] = %+v, want the user bubble", state.Messages[0])
	}
	if state.Messages[1].Role != "system" {
		t.Errorf("msg[1] role = %q, want system directive", state.Messages[1].Role)
	}
	// The directive is the steering text (its mid_turn_inject source rides the
	// event payload to the client, checked separately).
	if state.Messages[1].Content != midTurnInjectDirective {
		t.Errorf("directive content mismatch:\n%s", state.Messages[1].Content)
	}
}

// The loop re-runs one turn for a late inject row, then stops — mirroring the
// real flow where the follow-up turn's drain removes the row so refire flips to
// false. It must NOT loop forever even if the very first check reports pending.
func TestInject_LoopRefiresThenStops(t *testing.T) {
	var mu sync.Mutex
	var runs int
	r := newTestRunner(func(_ context.Context, _ runtime.TurnInput) error {
		mu.Lock()
		runs++
		mu.Unlock()
		return nil
	})
	// First check: a late row is waiting → refire. After the follow-up turn the
	// drain consumed it → false. (The real midTurnRefire flips the same way.)
	var calls int
	r.midTurnRefire = func(_ string) bool {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return calls == 1
	}

	r.SubmitUserTurn(runtime.TurnInput{AppID: "a", SessionID: "s1", ClientMessageID: "c1"},
		func() (uint64, error) { return 1, nil })

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		n := runs
		mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			t.Fatalf("runs=%d, want 2 (initial + one refire)", runs)
			mu.Unlock()
		case <-time.After(5 * time.Millisecond):
		}
	}
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if runs != 2 {
		t.Fatalf("runs=%d, want exactly 2 (refire once, then stop)", runs)
	}
}

// A turn that ERRORS (rate limit, provider error) must NOT refire even if an
// inject row is still pending — re-running would just re-hit the failure. The
// row stays queued for a later turn.
func TestInject_NoRefireOnTurnError(t *testing.T) {
	var mu sync.Mutex
	var runs int
	r := newTestRunner(func(_ context.Context, _ runtime.TurnInput) error {
		mu.Lock()
		runs++
		mu.Unlock()
		return errors.New("rate limited")
	})
	// refire would say "yes, a row is pending" — the error gate must veto it.
	r.midTurnRefire = func(_ string) bool { return true }

	r.SubmitUserTurn(runtime.TurnInput{AppID: "a", SessionID: "s1"},
		func() (uint64, error) { return 1, nil })
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if runs != 1 {
		t.Fatalf("runs=%d, want 1 (a failed turn must never refire)", runs)
	}
}

// A nil refire hook keeps the loop's behaviour identical — the guarantee that
// queue-only apps are unaffected.
func TestInject_NilRefireIsInert(t *testing.T) {
	var runs int
	var mu sync.Mutex
	r := newTestRunner(func(_ context.Context, _ runtime.TurnInput) error {
		mu.Lock()
		runs++
		mu.Unlock()
		return nil
	})
	// midTurnRefire left nil.
	r.SubmitUserTurn(runtime.TurnInput{AppID: "a", SessionID: "s1"},
		func() (uint64, error) { return 1, nil })
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if runs != 1 {
		t.Fatalf("runs=%d, want 1 (nil refire must not re-run)", runs)
	}
}
