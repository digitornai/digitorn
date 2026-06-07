package llmaudio

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/voice"
)

type fakeAudio struct {
	gotTranscribe *llm.TranscribeRequest
	gotSpeak      *llm.SpeechRequest
	transcript    string
	speakPCM      []byte
}

func (f *fakeAudio) Transcribe(_ context.Context, req *llm.TranscribeRequest) (<-chan *llm.AudioFrame, error) {
	f.gotTranscribe = req
	ch := make(chan *llm.AudioFrame, 2)
	ch <- llm.TextFrame("hel")
	ch <- llm.FinalFrame(f.transcript)
	close(ch)
	return ch, nil
}

func (f *fakeAudio) Speak(_ context.Context, req *llm.SpeechRequest) (<-chan *llm.AudioFrame, error) {
	f.gotSpeak = req
	ch := make(chan *llm.AudioFrame, 2)
	ch <- llm.AudioBytesFrame(f.speakPCM)
	ch <- llm.DoneFrame()
	close(ch)
	return ch, nil
}

// TestSTT proves an utterance is wrapped as WAV, sent through the gateway STT, and
// the streamed transcript surfaces as interim + final.
func TestSTT(t *testing.T) {
	fk := &fakeAudio{transcript: "hello world"}
	stt := NewSTT(fk, STTConfig{Model: "whisper-1", SampleRate: 16000, Route: Route{UserJWT: "jwt"}})
	s, err := stt.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Write(voice.Frame{Samples: make([]int16, 320), Rate: 16000})
	s.Endpoint()

	var final string
	deadline := time.After(time.Second)
	for final == "" {
		select {
		case tr := <-s.Results():
			if tr.Final {
				final = tr.Text
			}
		case <-deadline:
			t.Fatal("no final transcript")
		}
	}
	if final != "hello world" {
		t.Fatalf("final = %q", final)
	}
	// The request carried a valid WAV (RIFF header) + the routing JWT, format wav.
	req := fk.gotTranscribe
	if req == nil || req.Format != "wav" || req.UserJWT != "jwt" || string(req.Audio[:4]) != "RIFF" {
		t.Fatalf("transcribe request = %+v", req)
	}
}

// TestTTS proves a clause is spoken via the gateway and the PCM bytes decode into
// frames at the configured rate.
func TestTTS(t *testing.T) {
	// 4 samples of PCM16 (8 bytes); runtime conversion so negatives wrap, not overflow.
	want := []int16{100, -100, 200, -200}
	pcm := make([]byte, 2*len(want))
	for i, v := range want {
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(v))
	}
	fk := &fakeAudio{speakPCM: pcm}
	tts := NewTTS(fk, TTSConfig{Model: "tts-1", Voice: "alloy", SampleRate: 24000, Route: Route{UserJWT: "jwt"}})

	ch, err := tts.Synthesize(context.Background(), "hello there")
	if err != nil {
		t.Fatal(err)
	}
	var samples []int16
	for f := range ch {
		if f.Rate != 24000 {
			t.Fatalf("rate = %d", f.Rate)
		}
		samples = append(samples, f.Samples...)
	}
	if len(samples) != len(want) {
		t.Fatalf("decoded %d samples, want %d", len(samples), len(want))
	}
	for i := range want {
		if samples[i] != want[i] {
			t.Fatalf("sample[%d] = %d, want %d", i, samples[i], want[i])
		}
	}
	if fk.gotSpeak == nil || fk.gotSpeak.Format != "pcm" || fk.gotSpeak.Voice != "alloy" {
		t.Fatalf("speak request = %+v", fk.gotSpeak)
	}
}
