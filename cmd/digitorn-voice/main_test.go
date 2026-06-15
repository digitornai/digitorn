package main

import (
	"net/url"
	"strings"
	"testing"
)

// wsURL must select the daemon brain by query param : Voie A (pipeline) passes
// the STT/TTS knobs ; Voie B (realtime) passes engine=realtime + model + agent.
// The endpoint path is identical — the adapter never embeds brain logic.
func TestWsURL_PipelineVsRealtime(t *testing.T) {
	base := config{
		BaseURL: "http://127.0.0.1:8000", AppID: "callapp", EntryAgent: "main",
		Rate: 8000, STTModel: "whisper-1", TTSModel: "tts-1", TTSVoice: "alloy", Language: "fr",
	}

	// Voie A (default)
	u, err := url.Parse(wsURL(base, "sess-1"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "ws" || !strings.HasSuffix(u.Path, "/sessions/sess-1/voice/audio") {
		t.Fatalf("bad pipeline url: %s", u)
	}
	q := u.Query()
	if q.Get("engine") != "" || q.Get("stt_model") != "whisper-1" || q.Get("tts_voice") != "alloy" || q.Get("language") != "fr" {
		t.Fatalf("pipeline params wrong: %v", q)
	}

	// Voie B
	rt := base
	rt.Engine = "realtime"
	rt.RealtimeModel = "gpt-realtime"
	u2, err := url.Parse(wsURL(rt, "sess-1"))
	if err != nil {
		t.Fatal(err)
	}
	q2 := u2.Query()
	if q2.Get("engine") != "realtime" || q2.Get("realtime_model") != "gpt-realtime" ||
		q2.Get("agent") != "main" || q2.Get("tts_voice") != "alloy" {
		t.Fatalf("realtime params wrong: %v", q2)
	}
	if q2.Get("stt_model") != "" || q2.Get("tts_model") != "" {
		t.Fatalf("pipeline-only params must not leak into realtime mode: %v", q2)
	}

	// wss upgrade for https daemons
	tls := rt
	tls.BaseURL = "https://daemon.example"
	if u3 := wsURL(tls, "s"); !strings.HasPrefix(u3, "wss://daemon.example/") {
		t.Fatalf("https must upgrade to wss: %s", u3)
	}
}
