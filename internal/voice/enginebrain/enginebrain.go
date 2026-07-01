// Package enginebrain is the voice pipeline's REAL brain when it runs INSIDE the
// daemon: it drives an in-process daemon turn (the full agent — gateway LLM, tools,
// gates, memory) and streams the assistant token-deltas to the clause pipeline. It
// is the in-process counterpart of daemonbrain (which talks to the daemon over HTTP):
// here there is no second daemon, the daemon IS the brain.
//
// It implements voice.TurnRunner against a decoupled Brain seam so the relay/abort
// logic is unit-tested with a fake; the concrete Brain (engine.Run + a session-bus
// subscription for EventAssistantDelta) is wired at bootstrap.
package enginebrain

import (
	"context"

	"github.com/digitornai/digitorn/internal/voice"
)

// Turn is one in-flight daemon turn. Deltas streams the assistant token-deltas and is
// closed when the turn ends; Err reports any turn failure once Deltas is closed.
type Turn interface {
	Deltas() <-chan string
	Err() error
}

// Brain starts a daemon turn for the caller's text on the bound session and returns
// its live delta stream. The concrete impl appends the user message, runs engine.Run
// in a goroutine, and bridges the session bus's EventAssistantDelta → Deltas, closing
// it on the turn-ended event. Abort cancels the in-flight turn (voice barge-in).
type Brain interface {
	StartTurn(ctx context.Context, text string) (Turn, error)
	Abort(ctx context.Context) error
}

// Runner adapts a Brain to voice.TurnRunner. Per call (bound to one session's Brain).
type Runner struct {
	brain Brain
}

// New builds a runner over a Brain.
func New(brain Brain) *Runner { return &Runner{brain: brain} }

// Run starts the turn for text and forwards its token-deltas until the turn ends.
// Cancelling ctx (barge-in) stops relaying immediately; Abort drops the daemon turn.
func (r *Runner) Run(ctx context.Context, text string, deltas chan<- string) error {
	t, err := r.brain.StartTurn(ctx, text)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-t.Deltas():
			if !ok {
				return t.Err()
			}
			if d == "" {
				continue
			}
			select {
			case deltas <- d:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// Abort cancels the in-flight daemon turn (voice barge-in).
func (r *Runner) Abort(ctx context.Context) error { return r.brain.Abort(ctx) }

var _ voice.TurnRunner = (*Runner)(nil)
