// Package llmaudio bridges the gateway-routed audio RPCs (the LLM worker's
// Transcribe / Speak, which go through bifrost → the gateway) onto the voice
// orchestration's provider interfaces (voice.STTEngine / voice.TTSEngine). It is the
// daemon-side seam: STT/TTS never touch a provider directly — everything is the
// gateway, exactly like chat. Audio bytes are PCM16 mono; this package owns the
// PCM↔frame and WAV-container conversions.
package llmaudio

import (
	"context"
	"encoding/binary"
	"log/slog"

	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/voice"
)

// Transcriber / Synthesizer are the slice of *llm.Client the adapters need (so tests
// inject a fake). *llm.Client satisfies both.
type Transcriber interface {
	Transcribe(ctx context.Context, req *llm.TranscribeRequest) (<-chan *llm.AudioFrame, error)
}

type Synthesizer interface {
	Speak(ctx context.Context, req *llm.SpeechRequest) (<-chan *llm.AudioFrame, error)
}

// Route carries the per-call gateway routing + identity (the session's JWT, ids).
type Route struct {
	UserJWT       string
	BYOK          bool
	APIKey        string
	BaseURL       string
	SessionID     string
	UserID        string
	AgentID       string
	CorrelationID string
}

// ── STT (Transcribe-backed) ──────────────────────────────────────────────────

// STTConfig configures the gateway STT.
type STTConfig struct {
	Model      string
	Language   string
	SampleRate int // the caller's PCM rate (utterance is wrapped as WAV at this rate)
	Route      Route
}

// STT implements voice.STTEngine over the worker's Transcribe (gateway).
type STT struct {
	cfg STTConfig
	tc  Transcriber
}

func NewSTT(tc Transcriber, cfg STTConfig) *STT {
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = 16000
	}
	return &STT{cfg: cfg, tc: tc}
}

func (e *STT) Open(ctx context.Context) (voice.STTStream, error) {
	return &sttStream{
		ctx:     ctx,
		cfg:     e.cfg,
		tc:      e.tc,
		results: make(chan voice.Transcript, 8),
	}, nil
}

// sttStream accumulates an utterance's PCM (Write, from the pipeline's single loop
// goroutine) and, on Endpoint, ships it to the gateway and streams the transcript
// back. Bifrost transcribes a complete audio file, so the utterance is wrapped in a
// minimal WAV container; the VAD bounds its length.
type sttStream struct {
	ctx     context.Context
	cfg     STTConfig
	tc      Transcriber
	buf     []int16
	results chan voice.Transcript
}

func (s *sttStream) Write(f voice.Frame) error {
	s.buf = append(s.buf, f.Samples...)
	return nil
}

func (s *sttStream) Endpoint() {
	if len(s.buf) == 0 {
		return
	}
	utt := s.buf
	s.buf = nil
	wav := wavPCM16Mono(utt, s.cfg.SampleRate)
	slog.Info("voice STT: endpoint → transcribe", "samples", len(utt), "wav_bytes", len(wav), "model", s.cfg.Model)
	go s.transcribe(wav)
}

func (s *sttStream) transcribe(wav []byte) {
	r := s.cfg.Route
	out, err := s.tc.Transcribe(s.ctx, &llm.TranscribeRequest{
		Model:         s.cfg.Model,
		Audio:         wav,
		Format:        "wav",
		Language:      s.cfg.Language,
		BYOK:          r.BYOK,
		APIKey:        r.APIKey,
		UserJWT:       r.UserJWT,
		BaseURL:       r.BaseURL,
		SessionID:     r.SessionID,
		UserID:        r.UserID,
		AgentID:       r.AgentID,
		CorrelationID: r.CorrelationID,
	})
	if err != nil {
		slog.Warn("voice STT: transcribe call failed", "err", err.Error())
		return
	}
	got := false
	for f := range out {
		switch f.Kind() {
		case llm.FrameText:
			s.emit(voice.Transcript{Text: f.Text(), Final: false})
		case llm.FrameFinal:
			slog.Info("voice STT: final transcript", "text", f.Text())
			got = true
			s.emit(voice.Transcript{Text: f.Text(), Final: true})
		case llm.FrameError:
			slog.Warn("voice STT: gateway error frame", "msg", f.Text())
		}
	}
	if !got {
		slog.Warn("voice STT: no final transcript from gateway")
	}
}

func (s *sttStream) emit(t voice.Transcript) {
	select {
	case s.results <- t:
	case <-s.ctx.Done():
	}
}

func (s *sttStream) Results() <-chan voice.Transcript { return s.results }
func (s *sttStream) Close() error                     { return nil }

// ── TTS (Speak-backed) ───────────────────────────────────────────────────────

// TTSConfig configures the gateway TTS. SampleRate is the rate the provider returns
// for "pcm" (OpenAI TTS pcm = 24000). TargetRate, when set, resamples the audio to the
// call's rate (e.g. 8000 for telephony) so the orchestrator/transport stay rate-consistent.
type TTSConfig struct {
	Model      string
	Voice      string
	Language   string
	SampleRate int
	TargetRate int
	Route      Route
}

// TTS implements voice.TTSEngine over the worker's Speak (gateway).
type TTS struct {
	cfg TTSConfig
	syn Synthesizer
}

func NewTTS(syn Synthesizer, cfg TTSConfig) *TTS {
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = 24000
	}
	return &TTS{cfg: cfg, syn: syn}
}

// Synthesize streams one clause to PCM16 frames. Frames are forwarded the instant
// they arrive off the gateway (time-to-first-audio), decoded from raw PCM bytes.
func (e *TTS) Synthesize(ctx context.Context, text string) (<-chan voice.Frame, error) {
	r := e.cfg.Route
	in, err := e.syn.Speak(ctx, &llm.SpeechRequest{
		Model:         e.cfg.Model,
		Text:          text,
		Voice:         e.cfg.Voice,
		Format:        "pcm",
		BYOK:          r.BYOK,
		APIKey:        r.APIKey,
		UserJWT:       r.UserJWT,
		BaseURL:       r.BaseURL,
		SessionID:     r.SessionID,
		UserID:        r.UserID,
		AgentID:       r.AgentID,
		CorrelationID: r.CorrelationID,
	})
	if err != nil {
		slog.Warn("voice TTS: speak call failed", "err", err.Error())
		return nil, err
	}
	slog.Info("voice TTS: synthesizing clause", "model", e.cfg.Model, "chars", len(text))
	out := make(chan voice.Frame, 16)
	srcRate := e.cfg.SampleRate
	dstRate := e.cfg.TargetRate
	if dstRate <= 0 {
		dstRate = srcRate
	}
	go func() {
		defer close(out)
		for f := range in {
			if f.Kind() != llm.FrameAudio {
				continue
			}
			samples := resamplePCM16(pcm16ToSamples(f.Payload()), srcRate, dstRate)
			if len(samples) == 0 {
				continue
			}
			select {
			case out <- voice.Frame{Samples: samples, Rate: dstRate}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// resamplePCM16 linearly resamples mono PCM16 from→to Hz. Per-frame and stateless —
// good enough for V1 telephony (the 20 ms boundary discontinuity is inaudible); a
// stateful polyphase resampler is a quality/bench follow-up. No-op when rates match.
func resamplePCM16(in []int16, from, to int) []int16 {
	if from == to || from <= 0 || to <= 0 || len(in) == 0 {
		return in
	}
	outLen := len(in) * to / from
	if outLen <= 0 {
		return nil
	}
	out := make([]int16, outLen)
	ratio := float64(from) / float64(to)
	last := len(in) - 1
	for i := range outLen {
		pos := float64(i) * ratio
		idx := int(pos)
		if idx >= last {
			out[i] = in[last]
			continue
		}
		frac := pos - float64(idx)
		out[i] = int16(float64(in[idx])*(1-frac) + float64(in[idx+1])*frac)
	}
	return out
}

// ── PCM / WAV codecs ─────────────────────────────────────────────────────────

// pcm16ToSamples decodes little-endian PCM16 bytes into int16 samples (a trailing
// odd byte, never emitted mid-stream by providers, is dropped).
func pcm16ToSamples(b []byte) []int16 {
	n := len(b) / 2
	s := make([]int16, n)
	for i := range n {
		s[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return s
}

// wavPCM16Mono wraps PCM16 mono samples in a minimal 44-byte WAV header so the STT
// provider receives a valid audio file.
func wavPCM16Mono(samples []int16, rate int) []byte {
	dataLen := len(samples) * 2
	buf := make([]byte, 44+dataLen)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:], uint32(36+dataLen))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:], 16) // fmt chunk size
	binary.LittleEndian.PutUint16(buf[20:], 1)  // PCM
	binary.LittleEndian.PutUint16(buf[22:], 1)  // mono
	binary.LittleEndian.PutUint32(buf[24:], uint32(rate))
	binary.LittleEndian.PutUint32(buf[28:], uint32(rate*2)) // byte rate
	binary.LittleEndian.PutUint16(buf[32:], 2)              // block align
	binary.LittleEndian.PutUint16(buf[34:], 16)             // bits/sample
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:], uint32(dataLen))
	for i, v := range samples {
		binary.LittleEndian.PutUint16(buf[44+2*i:], uint16(v))
	}
	return buf
}
