package voice

import (
	"context"
	"sync/atomic"
)

type Orchestrator struct {
	Engine Engine
	NewVAD func() VAD
}

func NewOrchestrator(engine Engine) *Orchestrator {
	return &Orchestrator{Engine: engine, NewVAD: func() VAD { return NewEnergyVAD() }}
}

type PlaybackClearer interface {
	ClearPlayback()
}

func (o *Orchestrator) Handle(ctx context.Context, call Call, opts SessionOpts) error {
	sess, err := o.Engine.Session(ctx, opts)
	if err != nil {
		return err
	}
	defer sess.Close()

	vad := o.NewVAD()

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

	go func() {
		for f := range sess.Out() {
			select {
			case call.Out() <- f:
			case <-ctx.Done():
				return
			}
		}
	}()

	prevSpeech := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case f, ok := <-call.In():
			if !ok {
				return nil
			}
			select {
			case sess.Audio() <- f:
			case <-ctx.Done():
				return ctx.Err()
			}
			speech, endpoint := vad.Push(f)
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
