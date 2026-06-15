package voice

import (
	"context"
	"sync/atomic"
)

// Orchestrator drives one call: it pumps decoded audio from the Transport's Call
// through the VAD into the Engine session, plays the engine's outbound audio back
// to the caller, commits a turn on endpoint, and triggers a HARD barge-in when the
// caller starts speaking while the agent is talking. It is transport- AND engine-
// agnostic: any Call × any Engine.
type Orchestrator struct {
	Engine Engine
	NewVAD func() VAD // a fresh VAD per call (stateful); defaults to EnergyVAD
}

// NewOrchestrator builds an orchestrator over an engine, with the default VAD.
func NewOrchestrator(engine Engine) *Orchestrator {
	return &Orchestrator{Engine: engine, NewVAD: func() VAD { return NewEnergyVAD() }}
}

// PlaybackClearer is an OPTIONAL Call capability : transports with a remote
// playback buffer (Twilio Media Streams buffers outbound audio on Twilio's side)
// implement it so a barge-in also flushes what the caller would otherwise keep
// hearing. Transports without such a buffer (raw WS, AudioSocket) simply don't
// implement it — the orchestrator probes with a type assertion.
type PlaybackClearer interface {
	ClearPlayback()
}

// Handle runs one call to completion (Call.In closes or ctx ends).
func (o *Orchestrator) Handle(ctx context.Context, call Call, opts SessionOpts) error {
	sess, err := o.Engine.Session(ctx, opts)
	if err != nil {
		return err
	}
	defer sess.Close()

	vad := o.NewVAD()

	// Track whether the agent is currently speaking (for barge-in edge detection).
	var agentSpeaking atomic.Bool
	go func() {
		for e := range sess.Events() {
			switch e.Kind {
			case EvSpeakingStart:
				agentSpeaking.Store(true)
			case EvSpeakingStop:
				agentSpeaking.Store(false)
			}
		}
	}()

	// Play engine output back to the caller.
	go func() {
		for f := range sess.Out() {
			select {
			case call.Out() <- f:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Pump caller audio → VAD → engine, with endpoint→commit and barge-in.
	prevSpeech := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case f, ok := <-call.In():
			if !ok {
				return nil // caller hung up
			}
			select {
			case sess.Audio() <- f:
			case <-ctx.Done():
				return ctx.Err()
			}
			speech, endpoint := vad.Push(f)
			// Barge-in: rising edge of caller speech while the agent is talking.
			// Cancel the engine's response AND flush the transport's remote
			// playback buffer (when it has one) so the caller stops hearing the
			// stale reply immediately.
			if speech && !prevSpeech && agentSpeaking.Load() {
				sess.Cancel()
				if pc, ok := call.(PlaybackClearer); ok {
					pc.ClearPlayback()
				}
			}
			prevSpeech = speech
			if endpoint {
				sess.Commit()
			}
		}
	}
}
