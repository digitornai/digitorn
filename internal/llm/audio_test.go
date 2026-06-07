package llm

import (
	"bytes"
	"testing"
)

// TestAudioFrameRoundtrip proves the tagged framing: kind + payload survive, and
// control frames carry their text.
func TestAudioFrameRoundtrip(t *testing.T) {
	pcm := []byte{0x01, 0x02, 0x03, 0x04}
	if f := AudioBytesFrame(pcm); f.Kind() != FrameAudio || !bytes.Equal(f.Payload(), pcm) {
		t.Fatalf("audio frame = kind %#x payload %v", f.Kind(), f.Payload())
	}
	if f := TextFrame("hel"); f.Kind() != FrameText || f.Text() != "hel" {
		t.Fatalf("text frame = %+v", f)
	}
	if f := FinalFrame("hello world"); f.Kind() != FrameFinal || f.Text() != "hello world" {
		t.Fatalf("final frame = %+v", f)
	}
	if f := ErrorFrame("boom"); f.Kind() != FrameError || f.Text() != "boom" {
		t.Fatalf("error frame = %+v", f)
	}
	if f := DoneFrame(); f.Kind() != FrameDone {
		t.Fatalf("done frame kind = %#x", f.Kind())
	}
	// Empty frame degrades to Done, never panics.
	empty := &AudioFrame{}
	if empty.Kind() != FrameDone || empty.Payload() != nil {
		t.Fatalf("empty frame = %+v", empty)
	}
}

// TestAudioCodec proves the raw-passthrough path (audio frames travel as their bytes,
// no base64) and the JSON fallback (the one-shot request), and that Unmarshal owns
// its bytes (no aliasing of a reused gRPC buffer).
func TestAudioCodec(t *testing.T) {
	c := audioCodec{}

	frame := AudioBytesFrame([]byte{0x10, 0x20, 0x30})
	raw, err := c.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	if !bytes.Equal(raw, frame.Data) {
		t.Fatalf("frame did not pass through raw: %v vs %v", raw, frame.Data)
	}

	// Unmarshal into a fresh frame from a buffer we then mutate — the codec must
	// have copied, so the decoded frame is unaffected.
	buf := append([]byte(nil), raw...)
	var got AudioFrame
	if err := c.Unmarshal(buf, &got); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	for i := range buf {
		buf[i] = 0xFF
	}
	if !bytes.Equal(got.Data, raw) {
		t.Fatalf("codec aliased the receive buffer: %v", got.Data)
	}

	// Non-frame messages fall back to JSON.
	req := &SpeechRequest{Model: "tts-1", Text: "hi", Voice: "alloy"}
	enc, err := c.Marshal(req)
	if err != nil || enc[0] != '{' {
		t.Fatalf("request should be JSON-encoded, got %q err %v", enc, err)
	}
	var back SpeechRequest
	if err := c.Unmarshal(enc, &back); err != nil || back.Text != "hi" {
		t.Fatalf("request roundtrip failed: %+v err %v", back, err)
	}
}
