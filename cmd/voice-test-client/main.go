// Command voice-test-client drives the daemon's voice endpoint end-to-end WITHOUT
// Asterisk: it creates a session, synthesizes a spoken question via the gateway TTS,
// streams that audio (PCM) into the daemon's /voice/audio WebSocket followed by
// silence (to trip the VAD endpoint), reads the agent's spoken reply back as PCM,
// saves it to a WAV, and transcribes it via the gateway STT so you can READ what the
// agent said. One process proves: WS → STT → agent turn → TTS → WS.
//
// Flags: -daemon, -gateway, -app, -text, -rate, -voice, -stt-model, -tts-model.
// Auth: -token or ~/.digitorn/credentials.json access_token.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	daemon := flag.String("daemon", "http://127.0.0.1:8000", "daemon base URL")
	gateway := flag.String("gateway", "http://127.0.0.1:8002", "gateway base URL")
	app := flag.String("app", "", "app id (default: first installed app)")
	text := flag.String("text", "What is two plus two? Answer in one short sentence.", "spoken question")
	rate := flag.Int("rate", 24000, "PCM sample rate (24000 matches gateway TTS)")
	voice := flag.String("voice", "alloy", "TTS voice")
	sttModel := flag.String("stt-model", "whisper-1", "gateway STT model")
	ttsModel := flag.String("tts-model", "tts-1", "gateway TTS model")
	engine := flag.String("engine", "", "daemon voice engine: \"\" (Voie A pipeline) or \"realtime\" (Voie B)")
	realtimeModel := flag.String("realtime-model", "gpt-4o-realtime-preview", "gateway realtime model (engine=realtime)")
	idle := flag.Int("idle", 3, "seconds of silence before the reply is considered complete (raise for tool-call turns)")
	token := flag.String("token", "", "bearer token (default: credentials.json)")
	flag.Parse()

	jwt := *token
	if jwt == "" {
		jwt = loadToken()
	}
	if jwt == "" {
		die("no token (set -token or ~/.digitorn/credentials.json)")
	}
	c := &client{daemon: strings.TrimRight(*daemon, "/"), gateway: strings.TrimRight(*gateway, "/"), jwt: jwt}

	appID := *app
	if appID == "" {
		appID = c.firstApp()
	}
	fmt.Println("app:", appID)

	sid := c.createSession(appID)
	fmt.Println("session:", sid)

	fmt.Printf("synthesizing question: %q\n", *text)
	question := c.tts(*text, *voice, *ttsModel, *rate) // PCM16 @ rate
	fmt.Printf("question audio: %d samples (%.1fs)\n", len(question), float64(len(question))/float64(*rate))

	if *engine == "realtime" {
		fmt.Printf("engine: realtime (Voie B) · model %q\n", *realtimeModel)
	}
	fmt.Println("connecting voice WS + streaming...")
	reply := c.voiceExchange(appID, sid, question, *rate, *sttModel, *ttsModel, *voice, *engine, *realtimeModel, *idle)
	fmt.Printf("reply audio: %d samples (%.1fs)\n", len(reply), float64(len(reply))/float64(*rate))
	if len(reply) == 0 {
		die("no audio reply received — the agent did not speak back")
	}

	out := "voice_reply.wav"
	writeWAV(out, reply, *rate)
	fmt.Println("saved reply:", out)

	fmt.Println("transcribing the agent's spoken reply...")
	said := c.stt(reply, *rate, *sttModel)
	fmt.Println("\n================ AGENT SAID ================")
	fmt.Println(said)
	fmt.Println("===========================================")
}

type client struct {
	daemon, gateway, jwt string
	hc                   http.Client
}

func (c *client) firstApp() string {
	var resp struct {
		Apps []struct {
			AppID string `json:"app_id"`
			ID    string `json:"id"`
		} `json:"apps"`
	}
	c.getJSON(c.daemon+"/api/apps", &resp)
	if len(resp.Apps) == 0 {
		die("no apps installed — install one or pass -app")
	}
	if resp.Apps[0].AppID != "" {
		return resp.Apps[0].AppID
	}
	return resp.Apps[0].ID
}

func (c *client) createSession(appID string) string {
	var resp struct {
		SessionID string `json:"session_id"`
	}
	c.postJSON(c.daemon+"/api/apps/"+neturl.PathEscape(appID)+"/sessions",
		map[string]any{"context": "You are on a live voice call. Reply in one short spoken sentence."}, &resp)
	if resp.SessionID == "" {
		die("session creation returned no id")
	}
	return resp.SessionID
}

// tts synthesizes text to PCM16 at rate via the gateway.
func (c *client) tts(text, voice, model string, rate int) []int16 {
	body, _ := json.Marshal(map[string]any{
		"model": model, "input": text, "voice": voice,
		"response_format": "pcm", "sample_rate": rate,
	})
	req, _ := http.NewRequest("POST", c.gateway+"/v1/audio/speech", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.jwt)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	must(err, "tts")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		die(fmt.Sprintf("tts %d: %s", resp.StatusCode, b))
	}
	raw, _ := io.ReadAll(resp.Body)
	return bytesToPCM(raw)
}

// stt transcribes PCM16 (wrapped as WAV) via the gateway.
func (c *client) stt(pcm []int16, rate int, model string) string {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "utt.wav")
	fw.Write(wavBytes(pcm, rate))
	mw.WriteField("model", model)
	mw.Close()
	req, _ := http.NewRequest("POST", c.gateway+"/v1/audio/transcriptions", &buf)
	req.Header.Set("Authorization", "Bearer "+c.jwt)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.hc.Do(req)
	must(err, "stt")
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var r struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(b, &r)
	if r.Text == "" {
		return fmt.Sprintf("(no transcript; raw: %s)", strings.TrimSpace(string(b)))
	}
	return r.Text
}

// voiceExchange streams the question PCM + trailing silence into the daemon voice WS
// and collects the reply PCM frames.
func (c *client) voiceExchange(appID, sid string, question []int16, rate int, sttModel, ttsModel, voice, engine, realtimeModel string, idleSec int) []int16 {
	q := neturl.Values{}
	q.Set("rate", fmt.Sprint(rate))
	q.Set("stt_model", sttModel)
	q.Set("tts_model", ttsModel)
	q.Set("tts_voice", voice)
	if engine != "" {
		q.Set("engine", engine)
	}
	if engine == "realtime" && realtimeModel != "" {
		q.Set("realtime_model", realtimeModel)
	}
	wsBase := strings.Replace(strings.Replace(c.daemon, "https://", "wss://", 1), "http://", "ws://", 1)
	u := wsBase + "/api/apps/" + neturl.PathEscape(appID) + "/sessions/" + neturl.PathEscape(sid) + "/voice/audio?" + q.Encode()
	conn, _, err := (&websocket.Dialer{HandshakeTimeout: 10 * time.Second}).Dial(u, http.Header{"Authorization": {"Bearer " + c.jwt}})
	must(err, "voice ws dial")
	defer conn.Close()

	// Reader: collect reply PCM until the socket is idle for a while after audio.
	if idleSec <= 0 {
		idleSec = 3
	}
	idle := time.Duration(idleSec) * time.Second
	replyCh := make(chan []int16, 1)
	go func() {
		var reply []int16
		// Generous initial window: the question itself streams in real-time, and a
		// tool-call turn adds a gated round-trip before the model resumes speaking.
		_ = conn.SetReadDeadline(time.Now().Add(40 * time.Second))
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				replyCh <- reply
				return
			}
			if mt == websocket.BinaryMessage {
				reply = append(reply, bytesToPCM(data)...)
				// Extend while audio flows; stop `idle` seconds after it ends. A
				// tool-call turn goes silent during the gated execution, so raise
				// -idle to bridge that gap and capture the post-tool answer.
				_ = conn.SetReadDeadline(time.Now().Add(idle))
			}
		}
	}()

	// Send the question in 20 ms frames, then ~800 ms of silence to trip the VAD.
	frame := rate / 50
	send := func(s []int16) {
		for i := 0; i < len(s); i += frame {
			end := i + frame
			if end > len(s) {
				end = len(s)
			}
			_ = conn.WriteMessage(websocket.BinaryMessage, pcmToBytes(s[i:end]))
			time.Sleep(20 * time.Millisecond) // pace like real-time audio
		}
	}
	send(question)
	send(make([]int16, rate*8/10)) // 800ms silence → endpoint → commit → turn

	return <-replyCh
}

// ── HTTP + codec helpers ─────────────────────────────────────────────────────

func (c *client) getJSON(url string, out any) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+c.jwt)
	resp, err := c.hc.Do(req)
	must(err, "GET "+url)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		die(fmt.Sprintf("GET %s → %d: %s", url, resp.StatusCode, b))
	}
	_ = json.Unmarshal(b, out)
}

func (c *client) postJSON(url string, body, out any) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+c.jwt)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	must(err, "POST "+url)
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		die(fmt.Sprintf("POST %s → %d: %s", url, resp.StatusCode, rb))
	}
	_ = json.Unmarshal(rb, out)
}

func bytesToPCM(b []byte) []int16 {
	n := len(b) / 2
	s := make([]int16, n)
	for i := 0; i < n; i++ {
		s[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return s
}

func pcmToBytes(s []int16) []byte {
	b := make([]byte, len(s)*2)
	for i, v := range s {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}

func wavBytes(samples []int16, rate int) []byte {
	dataLen := len(samples) * 2
	buf := make([]byte, 44+dataLen)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:], uint32(36+dataLen))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:], 16)
	binary.LittleEndian.PutUint16(buf[20:], 1)
	binary.LittleEndian.PutUint16(buf[22:], 1)
	binary.LittleEndian.PutUint32(buf[24:], uint32(rate))
	binary.LittleEndian.PutUint32(buf[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(buf[32:], 2)
	binary.LittleEndian.PutUint16(buf[34:], 16)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:], uint32(dataLen))
	for i, v := range samples {
		binary.LittleEndian.PutUint16(buf[44+2*i:], uint16(v))
	}
	return buf
}

func writeWAV(path string, samples []int16, rate int) {
	must(os.WriteFile(path, wavBytes(samples, rate), 0o644), "write wav")
}

func loadToken() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".digitorn", "credentials.json"))
	if err != nil {
		return ""
	}
	var creds struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(data, &creds)
	return creds.AccessToken
}

func must(err error, what string) {
	if err != nil {
		die(what + ": " + err.Error())
	}
}

func die(msg string) {
	fmt.Fprintln(os.Stderr, "ERROR:", msg)
	os.Exit(1)
}

var _ = context.Background
