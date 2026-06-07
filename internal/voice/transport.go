package voice

import "context"

// Transport is one call service (Twilio, Asterisk, FreeSWITCH, SIP, WebRTC,
// Plivo…). Serve accepts inbound calls and hands each to the orchestrator as a
// Call. It owns the wire codec (μ-law / L16 / Opus) so the orchestrator only ever
// sees decoded Frames — that is what makes the core support ANY call service.
type Transport interface {
	Name() string
	Serve(ctx context.Context, handler CallHandler) error
}

// CallHandler is invoked once per inbound call (the orchestrator).
type CallHandler func(ctx context.Context, c Call)

// Call is one live phone/voice call: decoded audio in/out + identity + lifecycle.
type Call interface {
	ID() string
	Caller() string
	In() <-chan Frame  // decoded inbound caller audio
	Out() chan<- Frame // audio to play back (the transport encodes it)
	Hangup() error
}
