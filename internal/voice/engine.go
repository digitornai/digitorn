package voice

import "context"

type Frame struct {
	Samples []int16
	Rate    int
}

type Transcript struct {
	Text  string
	Final bool
}

type EventKind int

const (
	EvPartial EventKind = iota
	EvFinal
	EvSpeakingStart
	EvSpeakingStop
	EvTurnDone
	EvError
)

type Event struct {
	Kind EventKind
	Text string
	Err  error
}

type SessionOpts struct {
	SampleRate int
	Context    string
}

type Engine interface {
	Session(ctx context.Context, opts SessionOpts) (Session, error)
}

type Session interface {
	Audio() chan<- Frame
	Commit()
	Out() <-chan Frame
	Events() <-chan Event
	Cancel()
	Close() error
}

type STTEngine interface {
	Open(ctx context.Context) (STTStream, error)
}

type STTStream interface {
	Write(Frame) error
	Endpoint()
	Results() <-chan Transcript
	Close() error
}

type TTSEngine interface {
	Synthesize(ctx context.Context, text string) (<-chan Frame, error)
}

type TurnRunner interface {
	Run(ctx context.Context, text string, deltas chan<- string) error
	Abort(ctx context.Context) error
}
