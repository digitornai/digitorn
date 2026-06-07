package llm

import (
	"github.com/bytedance/sonic"

	"google.golang.org/grpc/encoding"
)

// AudioCodecName is the content-subtype selected per-call for the audio RPCs. It is
// distinct from the "json" codec so audio frames travel as raw bytes while control
// messages (the one-shot request) still use JSON.
const AudioCodecName = "digitorn.audio"

// audioCodec carries an *AudioFrame as its bytes verbatim (zero base64, zero JSON —
// the latency-critical path) and falls back to sonic for any other message type
// (the one-shot SpeechRequest / TranscribeRequest at stream open). One codec, two
// behaviours selected by Go type, so a single RPC can mix a JSON request with raw
// audio frames.
type audioCodec struct{}

func (audioCodec) Marshal(v any) ([]byte, error) {
	if f, ok := v.(*AudioFrame); ok {
		return f.Data, nil
	}
	return sonic.Marshal(v)
}

func (audioCodec) Unmarshal(data []byte, v any) error {
	if f, ok := v.(*AudioFrame); ok {
		// gRPC may reuse the receive buffer after this returns, so own the bytes.
		f.Data = append([]byte(nil), data...)
		return nil
	}
	return sonic.Unmarshal(data, v)
}

func (audioCodec) Name() string { return AudioCodecName }

func init() { encoding.RegisterCodec(audioCodec{}) }
