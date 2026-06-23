package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
	"github.com/mbathepaul/digitorn/internal/voice"
	"github.com/mbathepaul/digitorn/internal/voice/busbrain"
	"github.com/mbathepaul/digitorn/internal/voice/enginebrain"
	"github.com/mbathepaul/digitorn/internal/voice/llmaudio"
	"github.com/mbathepaul/digitorn/internal/voice/realtime"
	wstransport "github.com/mbathepaul/digitorn/internal/voice/transport/ws"
)

// voiceSystemContext nudges the agent to answer in a spoken style. Injected per-call
// via SessionOpts (no markdown / lists / emojis on a phone line).
const voiceSystemContext = "You are on a live voice call. Reply in one or two short, spoken sentences. No markdown, no lists, no emojis."

// voiceAudioWS is the daemon-side voice endpoint: the digitorn-voice adapter connects
// here over a WebSocket and streams the call's audio. The daemon IS the brain — STT,
// the agent turn (gateway LLM + tools + gates + memory), and TTS all run here through
// the gateway; audio never leaves this process for a direct provider. One connection =
// one orchestrated conversation on the owned session.
//
// Provider/model selection comes as query params (the adapter reads them from the
// app's voice config): stt_model, tts_model, tts_voice, language, rate (call PCM rate).
func (d *Daemon) voiceAudioWS(w http.ResponseWriter, r *http.Request) {
	sid := sessionIDParam(r)
	appID := chi.URLParam(r, "app_id")
	if _, err := d.requireOwnedSession(r.Context(), sid); err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	uid := userIDOf(r.Context())
	jwt := extractBearer(r)

	q := r.URL.Query()
	callRate := atoiOr(q.Get("rate"), 8000)

	// Voie B (realtime speech-to-speech) : one provider model at the gateway
	// generates the audio ; the daemon supplies context + the gated toolset and
	// gates/executes every function call. Selected per-call via ?engine=realtime.
	if q.Get("engine") == "realtime" {
		d.voiceRealtime(w, r, appID, sid, uid, jwt, callRate)
		return
	}

	route := llmaudio.Route{UserJWT: jwt, SessionID: sid, UserID: uid}
	stt := llmaudio.NewSTT(d.llmClient, llmaudio.STTConfig{
		Model:      q.Get("stt_model"),
		Language:   q.Get("language"),
		SampleRate: callRate,
		Route:      route,
	})
	tts := llmaudio.NewTTS(d.llmClient, llmaudio.TTSConfig{
		Model:      q.Get("tts_model"),
		Voice:      q.Get("tts_voice"),
		Language:   q.Get("language"),
		SampleRate: 24000, // OpenAI TTS pcm rate
		TargetRate: callRate,
		Route:      route,
	})

	deps := busbrain.Deps{
		AppendUserMessage: func(ctx context.Context, text string) error {
			_, err := d.sessionStore.AppendDurable(ctx, sessionstore.Event{
				Type:      sessionstore.EventUserMessage,
				SessionID: sid,
				AppID:     appID,
				UserID:    uid,
				Message:   &sessionstore.MessagePayload{Role: "user", Content: text},
			})
			return err
		},
		Subscribe: func(cb func(sessionstore.Event)) (func(), error) {
			sub, err := d.sessionStore.Subscribe(sid, cb)
			if err != nil {
				return nil, err
			}
			return sub.Cancel, nil
		},
		Trigger: func() {
			d.sessionRunner.WakeTurn(runtime.TurnInput{AppID: appID, SessionID: sid, UserID: uid, UserJWT: jwt})
		},
		Abort: func() { d.sessionRunner.Abort(sid) },
	}

	runner := enginebrain.New(busbrain.New(deps))
	engine := voice.NewPipelineEngine(stt, tts, runner)
	orch := voice.NewOrchestrator(engine)
	opts := voice.SessionOpts{SampleRate: callRate, Context: voiceSystemContext}

	wstransport.Handler(func(ctx context.Context, call voice.Call) {
		_ = orch.Handle(ctx, call, opts)
	}).ServeHTTP(w, r)
}

// voiceRealtime serves Voie B : a single realtime model (reached through the
// gateway's /v1/realtime proxy) takes audio in and emits audio out, while the daemon
// stays the brain. The daemon assembles the session's instructions + curated gated
// toolset (Engine.VoiceContext, the SAME path as a text turn) and intercepts every
// function call the model makes, routing it through the gated executor
// (Engine.ExecuteToolGated, the SAME gates as a turn) before feeding the result back.
// No parallel logic : the realtime engine is a pure adapter onto the daemon's brain.
func (d *Daemon) voiceRealtime(w http.ResponseWriter, r *http.Request, appID, sid, uid, jwt string, callRate int) {
	q := r.URL.Query()
	orch, instructions, err := d.buildRealtimeOrchestrator(r.Context(), realtimeBinding{
		AppID:     appID,
		SessionID: sid,
		UserID:    uid,
		UserJWT:   jwt,
		Agent:     q.Get("agent"),
		Model:     q.Get("realtime_model"),
		TTSVoice:  q.Get("tts_voice"),
	})
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "realtime_unavailable", err.Error())
		return
	}
	slog.Info("voice realtime: starting (ws)", "app", appID, "session", sid)
	opts := voice.SessionOpts{SampleRate: callRate, Context: instructions}
	wstransport.Handler(func(ctx context.Context, call voice.Call) {
		_ = orch.Handle(ctx, call, opts)
	}).ServeHTTP(w, r)
}

// realtimeBinding identifies which app/session/user a realtime call drives, plus the
// model/voice/agent selection. It is the single input the realtime-orchestrator
// builder needs, so the SAME brain wiring backs both the WS endpoint and the
// telephony voice-server (Asterisk/Twilio) — only the transport differs.
type realtimeBinding struct {
	AppID     string
	SessionID string
	UserID    string
	UserJWT   string
	Agent     string
	Model     string
	TTSVoice  string
}

// buildRealtimeOrchestrator assembles the Voie B brain for one call: the gated,
// curated toolset + spoken instructions (VoiceContext — the SAME path as a text
// turn), a realtime engine that dials the gateway proxy and routes EVERY function
// call back through the daemon's gated executor (ExecuteToolGated), durable
// transcript persistence, and an orchestrator over it. Returns the orchestrator and
// the assembled instructions. Transport-agnostic: the caller drives it with any
// voice.Call (WebSocket, AudioSocket, Twilio, …).
func (d *Daemon) buildRealtimeOrchestrator(ctx context.Context, b realtimeBinding) (*voice.Orchestrator, string, error) {
	eng, ok := d.engine.(*runtime.Engine)
	if !ok {
		return nil, "", errors.New("realtime voice requires the runtime engine")
	}
	gatewayBase := strings.TrimRight(d.cfg.Workers.LLM.GatewayURL, "/")
	if gatewayBase == "" {
		return nil, "", errors.New("no gateway url configured for realtime voice")
	}
	sysPrompt, tools, err := eng.VoiceContext(ctx, b.AppID, b.Agent)
	if err != nil {
		return nil, "", err
	}
	rtTools := realtime.ToolsFunc{
		SpecsFn: func() []realtime.ToolSpec { return toRealtimeSpecs(tools) },
		ExecuteFn: func(ctx context.Context, callID, name, argsJSON string) (string, error) {
			args := map[string]any{}
			if strings.TrimSpace(argsJSON) != "" {
				_ = json.Unmarshal([]byte(argsJSON), &args)
			}
			out := eng.ExecuteToolGated(ctx, runtime.ToolInvocation{
				CallID:    callID,
				Name:      name,
				Args:      args,
				AppID:     b.AppID,
				SessionID: b.SessionID,
				UserID:    b.UserID,
				UserJWT:   b.UserJWT,
			})
			return outcomeJSON(out), nil
		},
	}
	dial := func(ctx context.Context, _ voice.SessionOpts) (realtime.Conn, error) {
		conn, derr := realtime.DialGateway(ctx, gatewayBase, b.UserJWT, b.Model)
		if derr != nil {
			slog.Warn("voice realtime: dial gateway failed", "err", derr.Error(), "gateway", gatewayBase, "model", b.Model)
		}
		return conn, derr
	}
	rtEngine := realtime.New(dial, rtTools, b.Model, b.TTSVoice)
	rtEngine.SetTranscriptSink(d.realtimeTranscriptSink(b.AppID, b.SessionID, b.UserID))

	instructions := sysPrompt
	if instructions == "" {
		instructions = voiceSystemContext
	} else {
		instructions += "\n\n" + voiceSystemContext
	}
	slog.Info("voice realtime: built orchestrator", "app", b.AppID, "session", b.SessionID, "model", b.Model, "tools", len(tools))
	return voice.NewOrchestrator(rtEngine), instructions, nil
}

// realtimeTranscriptSink persists a finalized user/assistant transcript to session
// history so /history mirrors the realtime call (in Voie B no daemon turn runs, so
// these are the only record of the conversation).
func (d *Daemon) realtimeTranscriptSink(appID, sid, uid string) func(role, text string) {
	return func(role, text string) {
		evType, msgRole := sessionstore.EventAssistantMessage, "assistant"
		if role == "user" {
			evType, msgRole = sessionstore.EventUserMessage, "user"
		}
		pctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, perr := d.sessionStore.AppendDurable(pctx, sessionstore.Event{
			Type:      evType,
			SessionID: sid,
			AppID:     appID,
			UserID:    uid,
			Message:   &sessionstore.MessagePayload{Role: msgRole, Content: text},
		}); perr != nil {
			slog.Warn("voice realtime: persist transcript failed", "role", role, "err", perr.Error())
		}
	}
}

// toRealtimeSpecs projects the daemon's curated toolset onto the realtime tool shape.
func toRealtimeSpecs(tools []llm.ToolSpec) []realtime.ToolSpec {
	out := make([]realtime.ToolSpec, 0, len(tools))
	for _, t := range tools {
		out = append(out, realtime.ToolSpec{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	return out
}

// outcomeJSON serializes a gated tool outcome into the function_call_output payload
// the realtime model reads back : the LLM-visible text + status (+ error when failed).
func outcomeJSON(o runtime.ToolOutcome) string {
	var text strings.Builder
	for _, p := range o.Parts {
		text.WriteString(p.Text)
	}
	m := map[string]any{"status": o.Status}
	if t := text.String(); t != "" {
		m["result"] = t
	}
	if o.Error != "" {
		m["error"] = o.Error
	}
	b, err := json.Marshal(m)
	if err != nil {
		return `{"status":"errored","error":"result serialization failed"}`
	}
	return string(b)
}

func atoiOr(s string, def int) int {
	if v, err := strconv.Atoi(s); err == nil && v > 0 {
		return v
	}
	return def
}
