package voice

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── fakes (any real provider plugs into these same interfaces) ──────────────

type fakeSTT struct{ final string }

func (f *fakeSTT) Open(context.Context) (STTStream, error) {
	return &fakeSTTStream{final: f.final, results: make(chan Transcript, 8)}, nil
}

type fakeSTTStream struct {
	final   string
	results chan Transcript
	writes  int32
}

func (s *fakeSTTStream) Write(Frame) error          { atomic.AddInt32(&s.writes, 1); return nil }
func (s *fakeSTTStream) Endpoint()                  { s.results <- Transcript{Text: s.final, Final: true} }
func (s *fakeSTTStream) Results() <-chan Transcript { return s.results }
func (s *fakeSTTStream) Close() error               { return nil }

type fakeTTS struct {
	mu      sync.Mutex
	clauses []string
}

func (f *fakeTTS) Synthesize(_ context.Context, text string) (<-chan Frame, error) {
	f.mu.Lock()
	f.clauses = append(f.clauses, text)
	f.mu.Unlock()
	ch := make(chan Frame, 1)
	ch <- Frame{Samples: make([]int16, len(text)), Rate: 8000}
	close(ch)
	return ch, nil
}

func (f *fakeTTS) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.clauses...)
}

type fakeRunner struct {
	tokens  []string
	gate    chan struct{} // if set, block after the first token (for barge-in tests)
	aborted atomic.Bool
}

func (f *fakeRunner) Run(ctx context.Context, _ string, deltas chan<- string) error {
	for i, t := range f.tokens {
		select {
		case deltas <- t:
		case <-ctx.Done():
			return ctx.Err()
		}
		if f.gate != nil && i == 0 {
			select {
			case <-f.gate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return nil
}

func (f *fakeRunner) Abort(context.Context) error { f.aborted.Store(true); return nil }

// ── helpers ─────────────────────────────────────────────────────────────────

func waitEvent(t *testing.T, s Session, kind EventKind, d time.Duration) Event {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case e := <-s.Events():
			if e.Kind == kind {
				return e
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event %d", kind)
		}
	}
}

// ── tests ───────────────────────────────────────────────────────────────────

func TestPipeline_TurnAndClauses(t *testing.T) {
	stt := &fakeSTT{final: "what is the weather"}
	tts := &fakeTTS{}
	runner := &fakeRunner{tokens: []string{"It ", "is ", "sunny. ", "Quite ", "warm ", "today."}}
	sess, err := NewPipelineEngine(stt, tts, runner).Session(context.Background(), SessionOpts{SampleRate: 8000})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	sess.Audio() <- Frame{Samples: make([]int16, 160), Rate: 8000}
	sess.Audio() <- Frame{Samples: make([]int16, 160), Rate: 8000}
	sess.Commit()

	// drain frames so synth never blocks
	var frames int32
	go func() {
		for range sess.Out() {
			atomic.AddInt32(&frames, 1)
		}
	}()

	if e := waitEvent(t, sess, EvFinal, time.Second); e.Text != "what is the weather" {
		t.Fatalf("final transcript = %q", e.Text)
	}
	waitEvent(t, sess, EvSpeakingStart, time.Second)
	waitEvent(t, sess, EvTurnDone, time.Second)

	// The clause-pipeline split the reply: TTS spoke clause-by-clause.
	if got := tts.got(); len(got) != 2 || got[0] != "It is sunny." || got[1] != "Quite warm today." {
		t.Fatalf("clauses spoken = %#v", got)
	}
	// The drainer goroutine consumes the buffered frames asynchronously.
	eventually(t, func() bool { return atomic.LoadInt32(&frames) > 0 }, time.Second, "no outbound audio frames produced")
}

func TestPipeline_BargeInAborts(t *testing.T) {
	stt := &fakeSTT{final: "tell me a long story"}
	tts := &fakeTTS{}
	runner := &fakeRunner{tokens: []string{"Once upon a time. ", "and it went on..."}, gate: make(chan struct{})}
	sess, err := NewPipelineEngine(stt, tts, runner).Session(context.Background(), SessionOpts{SampleRate: 8000})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	go func() {
		for range sess.Out() {
		}
	}()

	sess.Commit()
	waitEvent(t, sess, EvSpeakingStart, time.Second) // the agent is speaking

	// Caller barges in.
	sess.Cancel()

	deadline := time.After(time.Second)
	for !runner.aborted.Load() {
		select {
		case <-deadline:
			t.Fatal("barge-in did not abort the daemon turn")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestSegmenter(t *testing.T) {
	s := NewSegmenter()
	var got []string
	got = append(got, s.Push("Hello there. ")...)
	got = append(got, s.Push("How are ")...)
	got = append(got, s.Push("you? Fine")...)
	if len(got) != 2 || got[0] != "Hello there." || got[1] != "How are you?" {
		t.Fatalf("clauses = %#v", got)
	}
	if r := s.Flush(); r != "Fine" {
		t.Fatalf("flush = %q", r)
	}

	// Force-flush on length cap (a run with no punctuation).
	s2 := &Segmenter{MaxChars: 10}
	out := s2.Push("abcdefghijk")
	if len(out) != 1 {
		t.Fatalf("maxchars force-flush failed: %#v", out)
	}
}
