package enginebrain

import (
	"context"

	"github.com/digitornai/digitorn/internal/voice"
)

type Turn interface {
	Deltas() <-chan string
	Err() error
}

type Brain interface {
	StartTurn(ctx context.Context, text string) (Turn, error)
	Abort(ctx context.Context) error
}

type Runner struct {
	brain Brain
}

func New(brain Brain) *Runner { return &Runner{brain: brain} }

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

func (r *Runner) Abort(ctx context.Context) error { return r.brain.Abort(ctx) }

var _ voice.TurnRunner = (*Runner)(nil)
