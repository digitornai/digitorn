# Digitorn Voice — Design (BG-8-Voice)

Status: **AGREED (decisions locked with Paul)** · Date: 2026-06-07

The real-time speech-to-speech subsystem: a caller speaks, the agent answers in
voice, with phone-call latency. It is the last transport of the background/channel
family — but unlike webhook/cron/rss/telegram/whatsapp it is a **real-time media
pipeline**, not a request/response adapter, so it gets its own process and design.

---

## 0. Locked decisions

| # | Decision | Choice |
|---|---|---|
| 1 | Process topology | **Dedicated `cmd/digitorn-voice` binary** — own process, scales independently, isolates the real-time hot path from the batch job pool |
| 2 | Media transport | **Both** — Twilio Media Streams (telephony) **and** WebRTC (browser/app), Twilio first |
| 3 | STT/TTS on the hot path | **Direct to provider (BYOK)** for lowest latency; the gateway is the default/fallback for apps without BYOK |
| 4 | Barge-in | **Hard interrupt** — caller speech stops TTS immediately **and** aborts the daemon turn (reuses RT-6 interruption) |

---

## 1. Goals / non-goals

**Goals**
- **Phone-call latency.** First audio back to the caller < ~1.2 s after they stop speaking.
- **Perfect isolation.** Audio NEVER touches the daemon. The voice process invokes
  the daemon's public API (`POST /messages`) + subscribes to its Socket.IO reply
  stream — exactly like the background service. Daemon untouched.
- **Reuse everything.** BG-3 (daemon client), BG-4 (channel pipeline), the session
  strategy, AND the `entry_agent`/`context` passthrough we added — a voice session
  injects "spoken, concise" context so the agent adapts without knowing it's voice.
- **Pluggable engines.** `STTEngine` / `TTSEngine` interfaces; BYOK per app + a
  gateway-default; swap Cartesia/ElevenLabs/Deepgram/Whisper/Piper without core change.

**Non-goals (V1)**
- Multi-party calls / conferencing.
- On-device wake-word.
- Voice cloning / custom-voice training (use provider voices).

---

## 2. Topology

```
   PSTN phone ──Twilio──┐                browser/app ──WebRTC──┐
                        ▼                                      ▼
          ┌───────────────────────────────────────────────────────┐
          │  cmd/digitorn-voice   (dedicated real-time process)    │
          │                                                        │
          │  Media gateway  (Twilio WS μ-law │ Pion WebRTC Opus)   │
          │     │  audio frames                                    │
          │     ▼                                                  │
          │  VAD / endpointing ──► STT (BYOK direct, streaming)    │
          │     │ final transcript                                 │
          │     ▼                                                  │
          │  Call orchestrator ── POST /messages ──────────────┐   │
          │     ▲  assistant_delta (Socket.IO subscribe) ◄──────┼── │
          │     │                                              │   │
          │  Segmenter (clauses) ──► TTS (BYOK direct, stream) │   │
          │     │ audio chunks                                 │   │
          │     ▼                                              │   │
          │  Playback ──► caller        Barge-in ──► stop TTS + │   │
          │                              POST /abort (RT-6)     │   │
          └──────────────────────────────────────────────────┼───┘
                       daemon PUBLIC API (unchanged)          │
                                                              ▼
                                                   ┌────────────────────┐
                                                   │  digitornd (UNTOUCHED) │
                                                   │  POST /sessions        │
                                                   │  POST .../messages     │
                                                   │  POST .../abort        │
                                                   │  Socket.IO reply stream│
                                                   └────────────────────┘
```

---

## 3. The hot path (per call)

1. **Call in.** Twilio webhook returns TwiML `<Connect><Stream wss://voice/.../media>`,
   or WebRTC signaling completes (SDP offer/answer). A `Call` orchestrator goroutine starts.
2. **Session.** Create one daemon session per call (`shared` strategy, key `voice-<callId>`),
   with `context = "You are on a live voice call. Reply in one or two short spoken
   sentences. No markdown, no lists."` — via the createSession `context` passthrough.
3. **Listen.** Inbound audio frames → resample → **VAD**. Stream to **STT** (partials
   for UX, final on endpoint). Endpoint = configurable trailing silence (~300–500 ms).
4. **Turn.** On STT final → `POST /messages` (text). Subscribe the session's Socket.IO
   room; receive `assistant_delta` chunks.
5. **Speak.** Feed deltas to the **Segmenter**: flush a clause on sentence-final
   punctuation OR a length/time cap. Each clause → **TTS** streaming → audio chunks →
   resample → **playback** to the caller. First clause starts speaking while the LLM is
   still generating → this is the latency win.
6. **Barge-in (hard).** VAD detects caller speech during playback → **stop TTS + flush
   the playback buffer immediately**, `POST /abort` the in-flight turn, and re-open STT
   for the new utterance.
7. **End.** Hangup / WS close → close the daemon session, release the call's goroutines.

---

## 4. Latency budget (target: first audio < ~1.2 s)

| Stage | Budget | Lever |
|---|---|---|
| endpointing (silence detect) | ~300 ms | tunable threshold; partial-based early-commit |
| STT final after endpoint | ~100–300 ms | streaming STT (already mostly transcribed) |
| daemon turn TTFT (LLM) | 200 ms–1 s | **the variable** — mitigated by clause-pipeline |
| first clause → first TTS audio | ~100–200 ms | low-latency TTS (Cartesia ~90 ms TTFB) |

The clause-pipeline means we DON'T wait for the full reply: TTS speaks sentence 1
while the LLM writes sentence 2. Perceived latency ≈ endpoint + STT + TTFT + TTS-first.

---

## 5. Components (what's new)

### 5.1 Transport seam — support ANY call service

**Requirement (Paul): support every type of call and existing service** (Twilio,
**Asterisk**, FreeSWITCH, SIP trunks, WebRTC, Plivo/Vonage/Telnyx…), to be
configured by the dozen. So the media transport is a **pluggable seam**, exactly
like the engine/provider seam — the orchestrator is `Transport × Engine`, both
swappable.

```go
// Transport is one call service. Serve accepts inbound calls and hands each to the
// orchestrator as a Call (decoded audio in/out + lifecycle). Long-lived until ctx ends.
type Transport interface {
    Name() string
    Serve(ctx context.Context, handler CallHandler) error
}
type CallHandler func(ctx context.Context, c Call)
type Call interface {
    ID() string
    Caller() string
    In() <-chan Frame   // decoded inbound audio (transport handles the wire codec)
    Out() chan<- Frame  // audio to play (transport encodes it back)
    Hangup() error
}
```

Each integration is one file implementing `Transport`, sharing a small **codec
layer** (μ-law ⇄ PCM16, L16, Opus ⇄ PCM via Pion, resample):

| Service | How it streams audio | Transport impl |
|---|---|---|
| **Twilio Media Streams** | WSS, base64 μ-law 8 kHz | WS server + μ-law codec |
| **Asterisk** | **AudioSocket** (TCP, raw L16 16 kHz) — simplest; or ARI externalMedia / AGI | TCP AudioSocket server |
| **FreeSWITCH** | `mod_audio_fork` (WS) or ESL | WS / ESL client |
| **SIP trunk (generic)** | SIP signaling + RTP | SIP stack + RTP (or via a media server) |
| **WebRTC** | RTP/Opus over SRTP | **Pion** (pure Go) |
| **Plivo / Vonage / Telnyx** | media-streaming WS (Twilio-like) | WS server + codec |

The orchestrator never knows which service it is — it only sees `Call` frames.
First impls to ship: **Twilio MS** (WS μ-law) and **Asterisk AudioSocket** (TCP L16,
trivial wire) cover both hosted + self-hosted PBX. WebRTC/SIP follow.

### 5.2 Audio codec/resample layer
- μ-law 8 kHz (Twilio) ↔ PCM16; resample 8 k↔16 k (STT) and TTS-rate→8 k. Opus
  (WebRTC) ↔ PCM. Small, pure-Go where possible (μ-law/resample); Opus via Pion.

### 5.3 VAD / endpointing
- Energy-based + hangover (or WebRTC-VAD). Emits `speech_start` (barge-in trigger)
  and `speech_end` (endpoint → commit turn). Tunable thresholds per app.

### 5.4 Provider-agnostic engine seam — pipeline AND realtime

**Requirement (Paul): support ANY audio provider and everything audio agents do
today.** That means TWO families, behind one seam:

- **Pipeline (daemon-brained).** Separate STT + TTS; the brain is the Digitorn
  daemon turn → full agent power (tools, gates, memory, multi-agent). Classic
  STT→LLM→TTS.
- **Realtime / speech-to-speech (provider-brained).** A single provider takes
  audio in and emits audio out directly (OpenAI Realtime, Gemini Live, Ultravox,
  Pipecat-style). Lowest latency. To keep Digitorn's capabilities, the provider's
  function-calls bridge back to Digitorn tools (optional `ToolBridge`).

The Call orchestrator drives ONE abstraction so it doesn't care which family a
provider is:

```go
// Engine is one call's brain. The orchestrator feeds it inbound audio + endpoint
// signals and reads outbound audio + events. Pipeline and realtime both implement it.
type Engine interface {
    Session(ctx context.Context, opts SessionOpts) (Session, error)
}
type Session interface {
    Audio() chan<- Frame       // inbound caller audio
    Commit()                   // endpoint reached (pipeline: run a turn; realtime: VAD hint)
    Out() <-chan Frame         // outbound audio to caller
    Events() <-chan Event      // transcript / speaking_start / speaking_stop / turn_done / error
    Cancel()                   // hard barge-in: stop output now + abort in-flight work
    Close() error
}

// Pipeline composes these; realtime providers implement Engine directly.
type STTEngine interface {
    Stream(ctx context.Context, audio <-chan Frame) (<-chan Transcript, error)
}
type Transcript struct { Text string; Final bool }
type TTSEngine interface {
    Synthesize(ctx context.Context, text string) (<-chan Frame, error)
}
// TurnRunner is the daemon brain for the pipeline engine (POST /messages, read
// assistant deltas via Socket.IO, POST /abort). Realtime engines don't use it.
type TurnRunner interface {
    Run(ctx context.Context, text string, deltas chan<- string) error
    Abort(ctx context.Context) error
}
```
- **Pipeline impls**: STT Deepgram/AssemblyAI/Azure/Whisper · TTS Cartesia/ElevenLabs/Azure/Piper.
- **Realtime impls**: OpenAI Realtime · Gemini Live · Ultravox · (any WS speech-to-speech).
- A **registry** maps a provider name → an Engine factory, so `tts.provider` /
  `stt.provider` / `realtime.provider` in app.yaml selects it.
- **Failover**: an Engine can wrap a primary + fallback provider (e.g. Cartesia →
  Azure) so one provider outage doesn't drop the call.

### 5.4.1 Routing rule — LLM via the gateway by default

Everything is configured in app.yaml; the resolution rule per layer:

| Layer | Default | BYOK (per-app opt-in) |
|---|---|---|
| **LLM (the brain)** | **ALWAYS via the daemon gateway** | per-app BYOK, like every other Digitorn LLM call — voice NEVER bypasses it |
| **STT** | gateway-default STT (when the gateway exposes it) | `stt.provider` + `stt.api_key` → direct provider (latency) |
| **TTS** | gateway-default TTS (when the gateway exposes it) | `tts.provider` + `tts.api_key` → direct provider (latency) |

The pipeline engine's brain IS the Digitorn daemon turn (`TurnRunner` → `POST
/messages`), so the LLM goes through the gateway by default automatically — the
voice config never even names a model. STT/TTS default to the gateway too; an app
sets `stt`/`tts` only to go BYOK-direct for the lowest latency.

**Honest dependency:** the gateway-default LLM works today (the daemon already
routes there). Gateway-default **STT/TTS** needs the gateway to grow streaming
STT/TTS endpoints — until then, voice STT/TTS uses BYOK. Either way it's config,
not code.

### 5.5 Segmenter (clause-pipeline)
- Accumulates delta tokens; flushes on `. ! ? ;` / newline, OR ≥ N chars / ≥ T ms,
  so the first audio starts ASAP and long clauses never stall.

### 5.6 Call orchestrator + barge-in
- One state machine per call: `listening → thinking → speaking → (barge-in) → listening`.
- Hard barge-in: on `speech_start` while `speaking`, cancel TTS context + clear
  playback + `POST /abort`, transition to `listening`.

---

## 6. Config (app.yaml — same channels block)
```yaml
tools:
  modules:
    channels:
      config:
        providers:
          hotline:
            adapter: voice
            config:
              transport: twilio        # twilio | webrtc
              inbound_path: /voice/twiml
              media_path: /voice/media # wss endpoint
              stt: { provider: deepgram, api_key: "{{secret.DEEPGRAM}}", language: fr }
              tts: { provider: cartesia, api_key: "{{secret.CARTESIA}}", voice: "..." }
              endpoint_silence_ms: 400
            activation:
              session: "voice-{{event.source}}"
              context: "Live voice call. Reply in one or two short spoken sentences."
              reply: auto
```
Discovered + armed by the existing discovery package (a new `voice` case), but the
voice adapter runs in `digitorn-voice`, not the background service.

---

## 7. Isolation, security, scale
- **Isolation invariant** unchanged: `internal/voice` imports nothing from the daemon;
  the daemon never imports it. Audio stays in the voice process.
- **Security**: Twilio signature validation on the TwiML webhook; provider API keys
  resolved like channel secrets (`{{secret.X}}`), never logged; optional call recording
  is opt-in + stored outside the daemon.
- **Scale**: one bounded set of goroutines per active call (no global thread explosion);
  a max-concurrent-calls cap; back-pressure drops/queues new calls past the cap.

---

## 8. Phased plan (prove the hard core first, with fakes)

1. **V-1 — Orchestration core + fakes (THE critical part).** `STTEngine`/`TTSEngine`
   interfaces, Segmenter, VAD interface, Call state machine, hard barge-in — with FAKE
   engines (echo STT, tone TTS) + a fake in-memory transport. Fully unit-tested, zero
   external deps. Proves the pipeline + barge-in + latency accounting deterministically.
2. **V-2 — Twilio Media Streams transport.** TwiML + WSS server + μ-law codec/resample.
   Live with a real phone number.
3. **V-3 — Real STT (Deepgram) + TTS (Cartesia), BYOK.** Wire real engines; measure
   the live latency budget.
4. **V-4 — Segmenter tuning + barge-in live.** Tune thresholds; prove hard interrupt feels natural.
5. **V-5 — WebRTC transport (Pion).** Browser/app calls.
6. **V-6 — `cmd/digitorn-voice` binary + discovery `voice` case + concurrency/scale + hardening.**

Each phase: tested + isolated, daemon untouched.
