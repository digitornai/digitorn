// Package realtime is the daemon-side brain for speech-to-speech (Voie B). A single
// provider model (OpenAI Realtime, reached THROUGH the gateway's realtime proxy)
// takes audio in and emits audio out; this engine drives that session and keeps
// Digitorn in control of the logic: it hands the model the curated, gated toolset
// and INTERCEPTS every function-call the model makes, routing it through the daemon's
// gated executor (SG-4) before feeding the result back. The model proposes; the
// daemon authorises + executes — same trust model as a text turn.
//
// It implements voice.Engine against two seams so the brain is unit-tested with
// fakes: Conn (the realtime event WebSocket to the gateway proxy) and Tools (the
// daemon's gated tool executor). Turn detection is OFF on the provider — the
// orchestrator's VAD drives Commit/Cancel, exactly like the pipeline engine.
package realtime

import (
	"context"
	"encoding/base64"
	"log/slog"
	"sync"

	"github.com/mbathepaul/digitorn/internal/voice"
)

// Conn is one realtime session's event channel to the gateway proxy. Events are the
// provider's JSON objects (decoded to maps); Send emits a client event.
type Conn interface {
	Send(event map[string]any) error
	Events() <-chan map[string]any
	Close() error
}

// ToolSpec is one tool offered to the realtime model (the gated, curated set).
type ToolSpec struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// Tools is the daemon's gated tool surface for the call. Specs is the curated set
// (SG-3); Execute runs ONE call through the gates (SG-4) + middleware and returns the
// JSON result. The realtime model never executes a tool itself.
type Tools interface {
	Specs() []ToolSpec
	Execute(ctx context.Context, callID, name, argsJSON string) (resultJSON string, err error)
}

// Engine opens realtime sessions. dial connects the gateway realtime proxy for one
// call; tools is the gated executor; model/voice configure the provider.
type Engine struct {
	dial  func(ctx context.Context, opts voice.SessionOpts) (Conn, error)
	tools Tools
	model string
	voice string
}

// New builds a realtime engine.
func New(dial func(ctx context.Context, opts voice.SessionOpts) (Conn, error), tools Tools, model, ttsVoice string) *Engine {
	return &Engine{dial: dial, tools: tools, model: model, voice: ttsVoice}
}

// Session opens one call's realtime conversation.
func (e *Engine) Session(ctx context.Context, opts voice.SessionOpts) (voice.Session, error) {
	conn, err := e.dial(ctx, opts)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(ctx)
	s := &session{
		conn:   conn,
		tools:  e.tools,
		ctx:    sctx,
		cancel: cancel,
		out:    make(chan voice.Frame, 256),
		events: make(chan voice.Event, 64),
		rate:   opts.SampleRate,
	}
	if err := s.configure(e.model, e.voice, opts.Context); err != nil {
		cancel()
		_ = conn.Close()
		return nil, err
	}
	go s.loop()
	return s, nil
}

type session struct {
	conn  Conn
	tools Tools

	ctx    context.Context
	cancel context.CancelFunc
	out    chan voice.Frame
	events chan voice.Event
	rate   int

	mu       sync.Mutex
	speaking bool
	audioIn  chan voice.Frame
	closed   sync.Once
}

// configure sends session.update: turn_detection OFF (our VAD drives turns), PCM16
// both ways, the spoken-style instructions, and the gated toolset.
func (s *session) configure(model, voice, instructions string) error {
	sess := map[string]any{
		"modalities":                []string{"audio", "text"},
		"instructions":              instructions,
		"voice":                     voice,
		"input_audio_format":        "pcm16",
		"output_audio_format":       "pcm16",
		"turn_detection":            nil,
		"input_audio_transcription": map[string]any{"model": "whisper-1"},
	}
	// model is fixed at connection time via the gateway URL (?model=…) ; the OpenAI
	// realtime protocol rejects a "model" field inside session.update, so we never
	// resend it here. _ = model keeps the signature stable for callers/tests.
	_ = model
	specs := s.tools.Specs()
	if len(specs) > 0 {
		sess["tools"] = toolDefs(specs)
	}
	slog.Info("realtime: session.update", "voice", voice, "tools", len(specs), "instructions_len", len(instructions))
	return s.conn.Send(map[string]any{"type": "session.update", "session": sess})
}

func (s *session) Audio() chan<- voice.Frame  { return s.audioSink() }
func (s *session) Out() <-chan voice.Frame    { return s.out }
func (s *session) Events() <-chan voice.Event { return s.events }

// audioSink returns a channel whose writes are appended to the realtime input buffer.
// A small per-session goroutine forwards frames so Audio() stays a plain channel.
func (s *session) audioSink() chan<- voice.Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.audioIn != nil {
		return s.audioIn
	}
	s.audioIn = make(chan voice.Frame, 64)
	go func() {
		for {
			select {
			case <-s.ctx.Done():
				return
			case f := <-s.audioIn:
				_ = s.conn.Send(map[string]any{
					"type":  "input_audio_buffer.append",
					"audio": base64.StdEncoding.EncodeToString(pcm16Bytes(f.Samples)),
				})
			}
		}
	}()
	return s.audioIn
}

// Commit: the VAD detected end-of-utterance → commit the buffer + request a response.
func (s *session) Commit() {
	_ = s.conn.Send(map[string]any{"type": "input_audio_buffer.commit"})
	_ = s.conn.Send(map[string]any{"type": "response.create"})
}

// Cancel: hard barge-in → cancel the in-flight response + clear the input buffer.
func (s *session) Cancel() {
	_ = s.conn.Send(map[string]any{"type": "response.cancel"})
	_ = s.conn.Send(map[string]any{"type": "input_audio_buffer.clear"})
	s.setSpeaking(false)
}

func (s *session) Close() error {
	s.closed.Do(func() {
		s.cancel()
		_ = s.conn.Close()
		close(s.out)
		close(s.events)
	})
	return nil
}

// loop consumes provider events: audio out, transcripts, and — the key part —
// function-call interception routed through the daemon's gated executor.
func (s *session) loop() {
	defer s.Close()
	for {
		select {
		case <-s.ctx.Done():
			return
		case ev, ok := <-s.conn.Events():
			if !ok {
				return
			}
			s.handle(ev)
		}
	}
}

func (s *session) handle(ev map[string]any) {
	et := str(ev, "type")
	switch et {
	case "response.audio.delta":
		if b, err := base64.StdEncoding.DecodeString(str(ev, "delta")); err == nil && len(b) > 0 {
			s.setSpeaking(true)
			s.emitFrame(voice.Frame{Samples: bytesToPCM16(b), Rate: s.rate})
		}
	case "response.audio.done", "response.cancelled":
		s.setSpeaking(false)
	case "conversation.item.input_audio_transcription.completed":
		if t := str(ev, "transcript"); t != "" {
			slog.Info("realtime: user transcript", "text", t)
			s.emit(voice.Event{Kind: voice.EvFinal, Text: t})
		}
	case "response.function_call_arguments.done":
		slog.Info("realtime: function call", "name", str(ev, "name"), "call_id", str(ev, "call_id"))
		s.handleFunctionCall(str(ev, "call_id"), str(ev, "name"), str(ev, "arguments"))
	case "response.done":
		s.setSpeaking(false)
		s.emit(voice.Event{Kind: voice.EvTurnDone})
	case "error", "response.failed":
		slog.Warn("realtime: provider error event", "type", et, "event", ev)
		s.emit(voice.Event{Kind: voice.EvError})
	default:
		// Diagnostic : surface the provider's control events (session.created,
		// session.updated, response.created, input_audio_buffer.committed, …) so the
		// flow is observable while Voie B is being brought up live.
		slog.Info("realtime: event", "type", et)
	}
}

// handleFunctionCall is the ToolBridge: the model PROPOSES a call → the daemon gates
// + executes it → the result is fed back and the model resumes.
func (s *session) handleFunctionCall(callID, name, argsJSON string) {
	out, err := s.tools.Execute(s.ctx, callID, name, argsJSON)
	if err != nil {
		out = `{"error":` + jsonString(err.Error()) + `}`
	}
	_ = s.conn.Send(map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{"type": "function_call_output", "call_id": callID, "output": out},
	})
	_ = s.conn.Send(map[string]any{"type": "response.create"})
}

func (s *session) emitFrame(f voice.Frame) {
	select {
	case s.out <- f:
	case <-s.ctx.Done():
	}
}

func (s *session) emit(e voice.Event) {
	select {
	case s.events <- e:
	case <-s.ctx.Done():
	}
}

func (s *session) setSpeaking(on bool) {
	s.mu.Lock()
	was := s.speaking
	s.speaking = on
	s.mu.Unlock()
	if on && !was {
		s.emit(voice.Event{Kind: voice.EvSpeakingStart})
	} else if !on && was {
		s.emit(voice.Event{Kind: voice.EvSpeakingStop})
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func pcm16Bytes(s []int16) []byte {
	b := make([]byte, len(s)*2)
	for i, v := range s {
		b[2*i] = byte(v)
		b[2*i+1] = byte(v >> 8)
	}
	return b
}

func bytesToPCM16(b []byte) []int16 {
	n := len(b) / 2
	s := make([]int16, n)
	for i := range n {
		s[i] = int16(b[2*i]) | int16(b[2*i+1])<<8
	}
	return s
}

// toolDefs translates the gated toolset to the realtime "function" tool shape.
func toolDefs(specs []ToolSpec) []map[string]any {
	out := make([]map[string]any, 0, len(specs))
	for _, t := range specs {
		params := t.Parameters
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"type":        "function",
			"name":        t.Name,
			"description": t.Description,
			"parameters":  params,
		})
	}
	return out
}

// jsonString quotes s 