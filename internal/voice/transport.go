package voice

import "context"

type Transport interface {
	Name() string
	Serve(ctx context.Context, handler CallHandler) error
}

type CallHandler func(ctx context.Context, c Call)

type Call interface {
	ID() string
	Caller() string
	In() <-chan Frame
	Out() chan<- Frame
	Hangup() error
}
