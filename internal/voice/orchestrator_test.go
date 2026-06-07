package voice

import (
	"context"
	"testing"
	"time"
)

type fakeCall struct {
	in  chan Frame
	out chan Frame
}

func (c *fakeCall) ID() string        { return "call1" }
func (c *fakeCall) Caller() string    { return "+33600000000" }
func (c *fakeCall) In() <-chan Frame  { return c.in }
func (c *fakeCall) Out() chan<- Frame { return c.out }
func (c *fakeCall) Hangup() error     { close(c.in); return nil }

func loud() Frame {
	s := make([]int16, 160)
	for i := range s {
		s[i] = 8000
	}
	return Frame{Samples: s, Rate: 8000}
}
func quiet() Frame { return Frame{Samples: make([]int16, 160), Rate: 8000} }

func fastVAD() func() VAD {
	return func() VAD { return &EnergyVAD{Threshold: 700, SilenceMs: 40} }
}

func eventually(t *testing.T, cond func() bool, d time.Duration, msg string) {
	t.Helper()
	deadline := time.After(d)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatal(msg)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestOrchestrator_FullCall proves the whole core, transport- and engine-agnostic:
// caller audio → VAD endpoint → turn → clause-pipelined TTS → audio played back.
func TestOrchestrator_FullCall(t *testing.T) {
	stt := &fakeSTT{final: "hello"}
	tts := &fakeTTS{}
	runner := &fakeRunner{tokens: []string{"Hi there. ", "How can I help?"}}
	o := NewOrchestrator(NewPipelineEngine(stt, tts, runner))
	o.NewVAD = fastVAD()

	call := &fakeCall{in: make(chan Frame, 64), out: make(chan Frame, 256)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = o.Handle(ctx, call, SessionOpts{SampleRate: 8000}) }()

	for i := 0; i < 3; i++ {
		call.in <- loud()
	}
	for i := 0; i < 4; i++ {
		call.in <- quiet() // trailing silence → endpoint → commit
	}

	select {
	case <-call.out:
	case <-time.After(2 * time.Second):
		t.Fatal("no audio played back to the caller")
	}
	eventually(t, func() bool { return len(tts.got()) >= 1 }, time.Second, "TTS never spoke a clause")
}

// TestOrchestrator_BargeIn proves the real barge-in: while the agent speaks, the
// caller talks over it → VAD detects speech → the turn is aborted.
func TestOrchestrator_BargeIn(t *testing.T) {
	stt := &fakeSTT{final: "tell me a story"}
	tts := &fakeTTS{}
	runner := &fakeRunner{tokens: []string{"Once upon a time. ", "and so on..."}, gate: make(chan struct{})}
	o := NewOrchestrator(NewPipelineEngine(stt, tts, runner))
	o.NewVAD = fastVAD()

	call := &fakeCall{in: make(chan Frame, 64), out: make(chan Frame, 256)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = o.Handle(ctx, call, SessionOpts{SampleRate: 8000}) }()

	// First utterance → turn → agent starts speaking.
	for i := 0; i < 3; i++ {
		call.in <- loud()
	}
	for i := 0; i < 4; i++ {
		call.in <- quiet()
	}
	select {
	case <-call.out: // first audio = agent is speaking
	case <-time.After(2 * time.Second):
		t.Fatal("agent never started speaking")
	}
	time.Sleep(20 * time.Millisecond) // let EvSpeakingStart register

	// Caller barges in over the agent.
	for i := 0; i < 3; i++ {
		call.in <- loud()
	}
	eventually(t, runner.aborted.Load, time.Second, "barge-in did not abort the in-flight turn")
}
