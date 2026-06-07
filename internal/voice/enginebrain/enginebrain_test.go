package enginebrain

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type fakeTurn struct {
	ch  chan string
	err error
}

func (t *fakeTurn) Deltas() <-chan string { return t.ch }
func (t *fakeTurn) Err() error            { return t.err }

type fakeBrain struct {
	turn    *fakeTurn
	started atomic.Bool
	aborted atomic.Bool
}

func (b *fakeBrain) StartTurn(context.Context, string) (Turn, error) {
	b.started.Store(true)
	return b.turn, nil
}
func (b *fakeBrain) Abort(context.Context) error { b.aborted.Store(true); return nil }

// TestRunner_StreamsReply proves the in-process brain relays the daemon turn's deltas
// until the turn ends, then returns the turn error (nil here).
func TestRunner_StreamsReply(t *testing.T) {
	turn := &fakeTurn{ch: make(chan string, 4)}
	b := &fakeBrain{turn: turn}
	r := New(b)

	deltas := make(chan string, 8)
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), "what is the weather", deltas) }()

	turn.ch <- "It "
	turn.ch <- "is sunny."
	close(turn.ch) // turn end

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not finish on turn end")
	}
	var got []string
	for {
		select {
		case d := <-deltas:
			got = append(got, d)
			continue
		default:
		}
		break
	}
	if len(got) != 2 || got[0] != "It " || got[1] != "is sunny." {
		t.Fatalf("relayed = %#v", got)
	}
	if !b.started.Load() {
		t.Fatal("turn was not started")
	}
}

// TestRunner_TurnError surfaces the turn's terminal error.
func TestRunner_TurnError(t *testing.T) {
	turn := &fakeTurn{ch: make(chan string), err: errors.New("gateway down")}
	close(turn.ch)
	r := New(&fakeBrain{turn: turn})
	if err := r.Run(context.Background(), "hi", make(chan string, 1)); err == nil || err.Error() != "gateway down" {
		t.Fatalf("expected turn error, got %v", err)
	}
}

// TestRunner_CancelStops proves a barge-in (ctx cancel) stops relaying.
func TestRunner_CancelStops(t *testing.T) {
	turn := &fakeTurn{ch: make(chan string)} // never sends → Run blocks
	r := New(&fakeBrain{turn: turn})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, "hi", make(chan string, 1)) }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled Run should return an error")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not stop Run")
	}
}

// TestRunner_Abort proves barge-in reaches the brain's abort.
func TestRunner_Abort(t *testing.T) {
	b := &fakeBrain{turn: &fakeTurn{ch: make(chan string)}}
	r := New(b)
	if err := r.Abort(context.Background()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if !b.aborted.Load() {
		t.Fatal("abort did not reach the brain")
	}
}
