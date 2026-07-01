// Command digitorn-voice is the voice ADAPTER: a thin audio pipe with zero brain.
// It accepts calls over a media transport (Asterisk AudioSocket) and bridges each
// call's PCM audio to the daemon's voice WebSocket endpoint. ALL the logic — STT,
// the agent turn (gateway LLM + tools + gates + memory), TTS — runs in the daemon;
// this process only moves audio bytes. It is an adapter, exactly like the channels
// and background adapters: protocol translation, no application logic.
//
// Configure via DIGITORN_VOICE_* env vars. Provider/model choices are passed to the
// daemon as query params (the daemon routes them through the gateway).
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	"github.com/digitornai/digitorn/internal/background/daemonclient"
	"github.com/digitornai/digitorn/internal/voice"
	"github.com/digitornai/digitorn/internal/voice/transport/audiosocket"
	"github.com/digitornai/digitorn/internal/voice/transport/twilio"
)

const voiceContext = "You are on a live voice call. Reply in one or two short, spoken sentences. No markdown, no lists, no emojis."

type config struct {
	BaseURL    string
	Token      string
	AppID      string
	EntryAgent string
	// Transport selects the call service this adapter listens for : "asterisk"
	// (AudioSocket TCP, the default) or "twilio" (Media Streams WebSocket — point
	// the <Connect><Stream url="wss://…"/> TwiML at TwilioAddr). The transport
	// owns the wire codec ; the bridge below only ever sees PCM16 frames.
	Transport    string
	AsteriskAddr string
	TwilioAddr   string
	Rate         int
	// Engine selects the daemon-side voice brain : "pipeline" (Voie A — STT →
	// agent turn → TTS, the default) or "realtime" (Voie B — one speech-to-speech
	// model through the gateway; the daemon still gates every tool call). The
	// adapter stays a pure audio pipe either way — this only changes the query
	// params it passes to the daemon endpoint.
	Engine        string
	RealtimeModel string
	STTModel      string
	TTSModel      string
	TTSVoice      string
	Language      string
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := config{
		BaseURL:       env("DIGITORN_VOICE_BASE_URL", "http://127.0.0.1:8000"),
		Token:         loadToken(),
		AppID:         os.Getenv("DIGITORN_VOICE_APP_ID"),
		EntryAgent:    os.Getenv("DIGITORN_VOICE_ENTRY_AGENT"),
		Transport:     env("DIGITORN_VOICE_TRANSPORT", "asterisk"),
		AsteriskAddr:  env("DIGITORN_VOICE_ASTERISK_ADDR", ":9092"),
		TwilioAddr:    env("DIGITORN_VOICE_TWILIO_ADDR", ":9093"),
		Rate:          atoiOr(os.Getenv("DIGITORN_VOICE_RATE"), 8000),
		Engine:        env("DIGITORN_VOICE_ENGINE", "pipeline"),
		RealtimeModel: os.Getenv("DIGITORN_VOICE_REALTIME_MODEL"),
		STTModel:      os.Getenv("DIGITORN_VOICE_STT_MODEL"),
		TTSModel:      os.Getenv("DIGITORN_VOICE_TTS_MODEL"),
		TTSVoice:      os.Getenv("DIGITORN_VOICE_TTS_VOICE"),
		Language:      os.Getenv("DIGITORN_VOICE_LANGUAGE"),
	}
	if cfg.AppID == "" {
		log.Error("voice: DIGITORN_VOICE_APP_ID is required")
		os.Exit(1)
	}

	client := daemonclient.New(cfg.BaseURL, cfg.Token)
	var transport voice.Transport
	var addr string
	switch strings.ToLower(cfg.Transport) {
	case "twilio":
		transport, addr = twilio.New(cfg.TwilioAddr, ""), cfg.TwilioAddr
	case "asterisk", "audiosocket", "":
		transport, addr = audiosocket.New(cfg.AsteriskAddr), cfg.AsteriskAddr
	default:
		log.Error("voice: unknown transport (asterisk|twilio)", "transport", cfg.Transport)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("voice: adapter serving", "transport", transport.Name(), "addr", addr,
		"app", cfg.AppID, "daemon", cfg.BaseURL, "rate", cfg.Rate, "engine", cfg.Engine)
	if err := transport.Serve(ctx, func(cctx context.Context, call voice.Call) {
		bridge(cctx, call, cfg, client, log)
	}); err != nil && ctx.Err() == nil {
		log.Error("voice: transport failed", "err", err.Error())
		os.Exit(1)
	}
}

// bridge creates a daemon session for the call and pipes its audio to/from the
// daemon's voice WebSocket. It runs until the call or the socket ends.
func bridge(ctx context.Context, call voice.Call, cfg config, client *daemonclient.Client, log *slog.Logger) {
	sess, err := client.CreateSession(ctx, cfg.AppID, daemonclient.CreateSessionRequest{
		Context:    voiceContext,
		EntryAgent: cfg.EntryAgent,
	})
	if err != nil {
		log.Error("voice: create session", "err", err.Error())
		return
	}

	conn, _, err := (&websocket.Dialer{HandshakeTimeout: 10 * time.Second}).DialContext(ctx, wsURL(cfg, sess.SessionID), authHeader(cfg.Token))
	if err != nil {
		log.Error("voice: daemon ws dial", "session", sess.SessionID, "err", err.Error())
		return
	}
	defer conn.Close()
	log.Info("voice: call bridged", "call", call.ID(), "session", sess.SessionID)

	// call.In → daemon WS (encode PCM16).
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case f, ok := <-call.In():
				if !ok {
					return
				}
				// Single writer goroutine (the read loop below never writes), so no
				// write lock is needed — gorilla allows one concurrent reader + writer.
				if err := conn.WriteMessage(websocket.BinaryMessage, encodePCM16(f.Samples)); err != nil {
					return
				}
			}
		}
	}()

	// daemon WS → call.Out (decode PCM16).
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.BinaryMessage || len(data) < 2 {
			continue
		}
		select {
		case call.Out() <- voice.Frame{Samples: decodePCM16(data), Rate: cfg.Rate}:
		case <-ctx.Done():
			return
		}
	}
}

// wsURL builds the daemon voice endpoint URL with the provider params. In
// realtime mode (Voie B) it selects the speech-to-speech brain and its model /
// agent ; in pipeline mode (Voie A) it passes the STT/TTS knobs. Both reach the
// SAME daemon endpoint — the brain choice is a query param, never adapter logic.
func wsURL(cfg config, sessionID string) string {
	base := strings.Replace(strings.Replace(cfg.BaseURL, "https://", "wss://", 1), "http://", "ws://", 1)
	q := neturl.Values{}
	q.Set("rate", strconv.Itoa(cfg.Rate))
	if strings.EqualFold(cfg.Engine, "realtime") {
		q.Set("engine", "realtime")
		if cfg.RealtimeModel != "" {
			q.Set("realtime_model", cfg.RealtimeModel)
		}
		if cfg.EntryAgent != "" {
			q.Set("agent", cfg.EntryAgent)
		}
		if cfg.TTSVoice != "" {
			q.Set("tts_voice", cfg.TTSVoice)
		}
	} else {
		if cfg.STTModel != "" {
			q.Set("stt_model", cfg.STTModel)
		}
		if cfg.TTSModel != "" {
			q.Set("tts_model", cfg.TTSModel)
		}
		if cfg.TTSVoice != "" {
			q.Set("tts_voice", cfg.TTSVoice)
		}
		if cfg.Language != "" {
			q.Set("language", cfg.Language)
		}
	}
	return base + "/api/apps/" + neturl.PathEscape(cfg.AppID) + "/sessions/" + neturl.PathEscape(sessionID) + "/voice/audio?" + q.Encode()
}

func authHeader(token string) http.Header {
	if token == "" {
		return nil
	}
	return http.Header{"Authorization": {"Bearer " + token}}
}

func encodePCM16(s []int16) []byte {
	b := make([]byte, len(s)*2)
	for i, v := range s {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}

func decodePCM16(b []byte) []int16 {
	n := len(b) / 2
	s := make([]int16, n)
	for i := range n {
		s[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return s
}

func loadToken() string {
	if t := os.Getenv("DIGITORN_VOICE_TOKEN"); t != "" {
		return t
	}
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
	if json.Unmarshal(data, &creds) != nil {
		return ""
	}
	return creds.AccessToken
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func atoiOr(s string, def int) int {
	if v, err := strconv.Atoi(s); err == nil && v > 0 {
		return v
	}
	return def
}
