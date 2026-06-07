package voice

import (
	"context"
	"sync"
)

// PipelineEngine is the daemon-brained engine: STT → TurnRunner (the daemon turn)
// → Segmenter → TTS. It keeps full agent power (tools, gates, memory) and hides
// LLM latency via the clause-pipeline. Provider choice is injected (any STT/TTS).
type PipelineEngine struct {
	deps Deps
}

// Deps groups the pluggable parts of the pipeline so any provider combination can
// be wired. STT/TTS are stateless factories; Runner is the per-call brain (one
// PipelineEngine per call, so a single Runner per engine is correct).
type Deps struct {
	STT    STTEngine
	TTS    TTSEngine
	Runner TurnRunner
}

// NewPipelineEngine builds a pipeline engine from injected providers.
func NewPipelineEngine(stt STTEngine, tts TTSEngine, runner TurnRunner) *PipelineEngine {
	return &PipelineEngine{deps: Deps{STT: stt, TTS: tts, Runner: runner}}
}

// Session opens one call's pipeline.
func (e *PipelineEngine) Session(ctx context.Context, opts SessionOpts) (Session, error) {
	sctx, cancel := context.WithCancel(ctx)
	stt, err := e.deps.STT.Open(sctx)
	if err != nil {
		cancel()
		return nil, err
	}
	s := &pipelineSession{
		deps:     e.deps,
		runner:   e.deps.Runner,
		stt:      stt,
		opts:     opts,
		ctx:      sctx,
		cancelFn: cancel,
		audioIn:  make(chan Frame, 64),
		out:      make(chan Frame, 256),
		events:   make(chan Event, 64),
		commitCh: make(chan struct{}, 1),
		cancelCh: make(chan struct{}, 1),
	}
	go s.loop()
	return s, nil
}

type pipelineSession struct {
	deps   Deps
	runner TurnRunner
	stt    STTStream
	opts   SessionOpts

	ctx      context.Context
	cancelFn context.CancelFunc

	audioIn  chan Frame
	out      chan Frame
	events   chan Event
	commitCh chan struct{}
	cancelCh chan struct{}

	mu         sync.Mutex
	turnCancel context.CancelFunc // current turn's cancel, for barge-in
	turns      sync.WaitGroup     // in-flight turns, so shutdown drains them before closing channels
	closeOnce  sync.Once
}

func (s *pipelineSession) Audio() chan<- Frame  { return s.audioIn }
func (s *pipelineSession) Out() <-chan Frame    { return s.out }
func (s *pipelineSession) Events() <-chan Event { return s.events }

func (s *pipelineSession) Commit() {
	select {
	case s.commitCh <- struct{}{}:
	default:
	}
}

func (s *pipelineSession) Cancel() {
	select {
	case s.cancelCh <- struct{}{}:
	default:
	}
}

func (s *pipelineSession) Close() error {
	s.closeOnce.Do(func() {
		s.cancelFn()
		_ = s.stt.Close()
	})
	return nil
}

// loop is the session's single owner goroutine: it multiplexes inbound audio,
// STT results, endpoint commits and barge-in cancels.
func (s *pipelineSession) loop() {
	results := s.stt.Results()
	for {
		select {
		case <-s.ctx.Done():
			s.turns.Wait() // let in-flight turns finish before closing channels (no send-on-closed)
			close(s.out)
			close(s.events)
			return
		case f := <-s.audioIn:
			_ = s.stt.Write(f)
		case tr, ok := <-results:
			if !ok {
				results = nil
				continue
			}
			if tr.Final {
				s.emit(Event{Kind: EvFinal, Text: tr.Text})
				s.abortTurn() // a new utterance supersedes any in-flight turn
				s.turns.Add(1)
				go s.runTurn(tr.Text)
			} else {
				s.emit(Event{Kind: EvPartial, Text: tr.Text})
			}
		case <-s.commitCh:
			s.stt.Endpoint()
		case <-s.cancelCh:
			s.abortTurn()
		}
	}
}

// runTurn drives one turn: brain deltas → segmenter → TTS → outbound audio. It runs
// under a per-turn context so a barge-in cancels it instantly.
func (s *pipelineSession) runTurn(text string) {
	defer s.turns.Done()
	tctx, cancel := context.WithCancel(s.ctx)
	s.setTurnCancel(cancel)
	defer cancel()

	deltas := make(chan string, 64)
	go func() {
		_ = s.runner.Run(tctx, text, deltas)
		close(deltas)
	}()

	seg := NewSegmenter()
	speaking := false
	speak := func(clause string) {
		if clause == "" {
			return
		}
		if !speaking {
			s.emit(Event{Kind: EvSpeakingStart})
			speaking = true
		}
		frames, err := s.deps.TTS.Synthesize(tctx, clause)
		if err != nil {
			return
		}
		for f := range frames {
			select {
			case s.out <- f:
			case <-tctx.Done():
				return
			}
		}
	}

	for {
		select {
		case <-tctx.Done():
			if speaking {
				s.emit(Event{Kind: EvSpeakingStop})
			}
			return
		case d, ok := <-deltas:
			if !ok {
				speak(seg.Flush())
				if speaking {
					s.emit(Event{Kind: EvSpeakingStop})
				}
				s.emit(Event{Kind: EvTurnDone})
				return
			}
			for _, clause := range seg.Push(d) {
				speak(clause)
			}
		}
	}
}

// abortTurn is the hard barge-in: cancel the in-flight turn and tell the brain to
// abort the daemon-side turn so no tokens are wasted.
func (s *pipelineSession) abortTurn() {
	s.mu.Lock()
	c := s.turnCancel
	s.turnCancel = nil
	s.mu.Unlock()
	if c != nil {
		c()
		_ = s.runner.Abort(context.WithoutCancel(s.ctx))
	}
}

func (s *pipelineSession) setTurnCancel(c context.CancelFunc) {
	s.mu.Lock()
	s.turnCancel = c
	s.mu.Unlock()
}

func (s *pipelineSession) emit(e Event) {
	select {
	case s.events <- e:
	case <-s.ctx.Done():
	}
}
