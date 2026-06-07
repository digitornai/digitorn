package busbrain

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

type recorder struct {
	cb        func(sessionstore.Event)
	appended  atomic.Value // string
	triggered atomic.Bool
	aborted   atomic.Bool
	unsubbed  atomic.Bool
}

func (r *recorder) deps() Deps {
	return Deps{
		AppendUserMessage: func(_ context.Context, text string) error {
			r.appended.Store(text)
			return nil
		},
		Subscribe: func(cb func(sessionstore.Event)) (func(), error) {
			r.cb = cb
			return func() { r.unsubbed.Store(true) }, nil
		},
		Trigger: func() { r.triggered.Store(true) },
		Abort:   func() { r.aborted.Store(true) },
	}
}

func delta(text string) sessionstore.Event {
	return sessionstore.Event{
		Type:    sessionstore.EventAssistantDelta,
		Message: &sessionstore.MessagePayload{Role: "assistant", Parts: []sessionstore.MessagePart{{Text: text}}},
	}
}

// TestBrain_StreamsTurn proves the full path: subscribe + append + trigger, deltas
// flow off the bus, turn-ended closes the stream cleanly + unsubscribes.
func TestBrain_StreamsTurn(t *testing.T) {
	r := &recorder{}
	brain := New(r.deps())

	turn, err := brain.StartTurn(context.Background(), "what is the weather")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	if got, _ := r.appended.Load().(string); got != "what is the weather" {
		t.Fatalf("appended = %q", got)
	}
	if !r.triggered.Load() {
		t.Fatal("turn was not triggered")
	}

	r.cb(delta("It "))
	r.cb(delta("is sunny."))
	r.cb(sessionstore.Event{Type: sessionstore.EventTurnEnded, Turn: &sessionstore.TurnPayload{Status: "done"}})

	var got []string
	deadline := time.After(time.Second)
	for {
		select {
		case d, ok := <-turn.Deltas():
			if !ok {
				if len(got) != 2 || got[0] != "It " || got[1] != "is sunny." {
					t.Fatalf("deltas = %#v", got)
				}
				if turn.Err() != nil {
					t.Fatalf("Err = %v, want nil", turn.Err())
				}
				if !r.unsubbed.Load() {
					t.Fatal("did not unsubscribe on turn end")
				}
				return
			}
			got = append(got, d)
		case <-deadline:
			t.Fatal("stream did not close on turn end")
		}
	}
}

// TestBrain_TurnErrored surfaces the errored turn's reason as Err.
func TestBrain_TurnErrored(t *testing.T) {
	r := &recorder{}
	turn, err := New(r.deps()).StartTurn(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	r.cb(sessionstore.Event{Type: sessionstore.EventTurnEnded, Turn: &sessionstore.TurnPayload{Status: "errored", Reason: "gateway down"}})
	<-turn.Deltas() // drains to closed
	if turn.Err() == nil || turn.Err().Error() != "gateway down" {
		t.Fatalf("Err = %v", turn.Err())
	}
}

// TestBrain_Abort proves barge-in reaches the abort closure.
func TestBrain_Abort(t *testing.T) {
	r := &recorder{}
	brain := New(r.deps())
	if err := brain.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !r.aborted.Load() {
		t.Fatal("abort closure not called")
	}
}
