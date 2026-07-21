package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

func msg(role string) sessionstore.Message { return sessionstore.Message{Role: role} }

// Detection: an injected message unanswered = last non-system message is user.
func TestInject_UnansweredDetection(t *testing.T) {
	cases := []struct {
		name string
		msgs []sessionstore.Message
		want bool
	}{
		{"empty", nil, false},
		{"user waiting", []sessionstore.Message{msg("assistant"), msg("user")}, true},
		{"answered", []sessionstore.Message{msg("user"), msg("assistant")}, false},
		{"user + directive still waiting", []sessionstore.Message{msg("assistant"), msg("user"), msg("system")}, true},
		{"answered then directive", []sessionstore.Message{msg("user"), msg("assistant"), msg("system")}, false},
		{"only system", []sessionstore.Message{msg("system"), msg("system")}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := lastTurnMessageIsUnansweredUser(c.msgs); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
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

// The loop re-runs one turn when refire says a late message is waiting, then
// stops (refire false the second time) — no infinite loop.
func TestInject_LoopRefiresOnce(t *testing.T) {
	var mu sync.Mutex
	var runs int
	r := newTestRunner(func(_ context.Context, _ runtime.TurnInput) error {
		mu.Lock()
		runs++
		mu.Unlock()
		return nil
	})
	// refire: true the first time (a message landed late), false after.
	var calls int
	r.midTurnRefire = func(_ string) bool {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return calls == 1
	}

	// First turn (idle path).
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
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if runs != 2 {
		t.Fatalf("runs=%d, want exactly 2 (refire must fire once, not loop)", runs)
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
