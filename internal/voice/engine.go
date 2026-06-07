// Package voice is the real-time speech-to-speech core of Digitorn. It is the
// brain-agnostic orchestration layer: a Call drives an Engine that turns inbound
// caller audio into outbound spoken audio. Two engine families plug into the same
// seam — a PIPELINE engine (STT → daemon turn → TTS, full agent power) and a
// REALTIME engine (a single speech-to-speech provider). The package depends on
// nothing from the daemon; transports (Twilio/WebRTC) and providers (Deepgram/
// Cartesia/OpenAI Realtime/…) plug in behind interfaces. This file is the seam.
package voice

import "context"

// Frame is one chunk of mono PCM16 audio at Rate Hz. Transports decode their wire
// codec (μ-law, Opus) into Frames; engines produce Frames the transport encodes back.
type Frame struct {
	Samples []int16
	Rate    int
}

// Transcript is an STT result; Final marks the end of an utterance.
type Transcript struct {
	Text  string
	Final bool
}

// EventKind enumerates the lifecycle signals a Session emits.
type EventKind int

const (
	EvPartial       EventKind = iota // interim transcript
	EvFinal                          // final transcript (a turn is committed)
	EvSpeakingStart                  // first outbound audio of a reply
	EvSpeakingStop                   // the reply finished (or was barged-in)
	EvTurnDone                       // the turn is fully complete
	EvError
)

// Event is a Session lifecycle signal.
type Event struct {
	Kind EventKind
	Text string
	Err  error
}

// SessionOpts configures one call's engine session.
type SessionOpts struct {
	SampleRate int
	Context    string // spoken-style hint handed to the brain ("reply in short spoken sentences")
}

// Engine is one call's brain factory. Pipeline and realtime engines both implement it.
type Engine interface {
	Session(ctx context.Context, opts SessionOpts) (Session, error)
}

// Session is one live call conversation. The Call orchestrator feeds inbound audio
// + endpoint signals and reads outbound audio + events. Cancel is the hard barge-in.
type Session interface {
	Audio() chan<- Frame  // inbound caller audio
	Commit()              // endpoint reached: pipeline runs a turn, realtime takes it as a VAD hint
	Out() <-chan Frame    // outbound audio to the caller
	Events() <-chan Event // lifecycle signals
	Cancel()              // hard barge-in: stop output now + abort in-flight work
	Close() error
}

// STTEngine opens streaming transcription sessions (pipeline engine only).
type STTEngine interface {
	Open(ctx context.Context) (STTStream, error)
}

// STTStream is one streaming ASR utterance pipe. Write pushes audio; Endpoint marks
// the utterance boundary so the provider flushes a Final; Results carries partials+finals.
type STTStream interface {
	Write(Frame) error
	Endpoint()
	Results() <-chan Transcript
	Close() error
}

// TTSEngine streams synthesized audio for one clause (pipeline engine only).
type TTSEngine interface {
	Synthesize(ctx context.Context, text string) (<-chan Frame, error)
}

// TurnRunner is the brain of the pipeline engine: it sends the user's text to the
// Digitorn daemon (POST /messages), streams the assistant reply token-by-token, and
// aborts the in-flight turn on barge-in (POST /abort). Realtime engines bypass it.
type TurnRunner interface {
	Run(ctx context.Context, text string, deltas chan<- string) error
	Abort(ctx context.Context) error
}
