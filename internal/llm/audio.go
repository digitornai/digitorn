package llm

import "time"

// AudioFrame is the raw streaming message for the audio RPCs (Speak / Transcribe /
// Realtime). It is carried by the "digitorn.audio" codec as its bytes verbatim —
// no JSON, no base64 — so a 20 ms PCM frame costs one copy and nothing else on the
// hot path. The first byte is a kind tag; the rest is the payload.
type AudioFrame struct {
	Data []byte
}

// Frame kinds (first byte of AudioFrame.Data).
const (
	FrameAudio byte = 0x10 // payload = raw audio bytes (provider format)
	FrameText  byte = 0x02 // payload = UTF-8 transcript text (interim)
	FrameFinal byte = 0x03 // payload = UTF-8 transcript text (final)
	FrameDone  byte = 0x00 // payload = empty; end of stream
	FrameError byte = 0x01 // payload = UTF-8 error message
)

// Kind returns the frame's tag (FrameDone for an empty/short frame).
func (f *AudioFrame) Kind() byte {
	if len(f.Data) == 0 {
		return FrameDone
	}
	return f.Data[0]
}

// Payload returns the bytes after the kind tag (nil for an empty frame).
func (f *AudioFrame) Payload() []byte {
	if len(f.Data) <= 1 {
		return nil
	}
	return f.Data[1:]
}

// Text returns the payload as a string (for FrameText / FrameFinal / FrameError).
func (f *AudioFrame) Text() string { return string(f.Payload()) }

// newFrame builds a tagged frame. The payload is copied into a single allocation
// alongside the tag so the result owns its bytes.
func newFrame(kind byte, payload []byte) *AudioFrame {
	b := make([]byte, 1+len(payload))
	b[0] = kind
	copy(b[1:], payload)
	return &AudioFrame{Data: b}
}

// AudioBytesFrame wraps raw audio. textFrame / finalFrame / errorFrame / DoneFrame
// build the control frames.
func AudioBytesFrame(audio []byte) *AudioFrame { return newFrame(FrameAudio, audio) }
func TextFrame(s string) *AudioFrame           { return newFrame(FrameText, []byte(s)) }
func FinalFrame(s string) *AudioFrame           { return newFrame(FrameFinal, []byte(s)) }
func ErrorFrame(msg string) *AudioFrame         { return newFrame(FrameError, []byte(msg)) }
func DoneFrame() *AudioFrame                    { return &AudioFrame{Data: []byte{FrameDone}} }

// SpeechRequest is the TTS request (text → streamed audio) routed through the
// gateway by default (BYOK=false). The audio comes back as FrameAudio frames.
type SpeechRequest struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model"`
	Text     string `json:"text"`
	Voice    string `json:"voice,omitempty"`
	// Format is the provider audio container/encoding (e.g. "pcm", "mp3", "wav",
	// "opus"). "pcm" (raw PCM16) is the lowest-latency choice for telephony.
	Format       string   `json:"format,omitempty"`
	Speed        *float64 `json:"speed,omitempty"`
	Instructions string   `json:"instructions,omitempty"`

	// Routing (mirrors ChatRequest): BYOK=false → gateway via UserJWT.
	BYOK    bool          `json:"byok,omitempty"`
	APIKey  string        `json:"api_key,omitempty"`
	UserJWT string        `json:"user_jwt,omitempty"`
	BaseURL string        `json:"base_url,omitempty"`
	Timeout time.Duration `json:"timeout,omitempty"`

	// Identity for gateway/provider tracing.
	SessionID     string `json:"session_id,omitempty"`
	UserID        string `json:"user_id,omitempty"`
	AgentID       string `json:"agent_id,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// TranscribeRequest is the STT request (audio → streamed transcript) routed through
// the gateway by default. Bifrost transcribes a complete utterance's audio; the
// pipeline's VAD delimits the utterance, so latency is bounded by endpointing, not
// by buffering the whole call. Results come back as FrameText / FrameFinal frames.
type TranscribeRequest struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model"`
	// Audio is the utterance PCM/encoded bytes; Format names the encoding.
	Audio    []byte `json:"audio,omitempty"`
	Format   string `json:"format,omitempty"`
	Language string `json:"language,omitempty"`

	BYOK    bool          `json:"byok,omitempty"`
	APIKey  string        `json:"api_key,omitempty"`
	UserJWT string        `json:"user_jwt,omitempty"`
	BaseURL string        `json:"base_url,omitempty"`
	Timeout time.Duration `json:"timeout,omitempty"`

	SessionID     string `json:"session_id,omitempty"`
	UserID        string `json:"user_id,omitempty"`
	AgentID       string `json:"agent_id,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}
