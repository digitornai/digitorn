# Digitorn Voice — Realtime (Voie B) Design

Status: **PROPOSED — awaiting Paul's validation** · Date: 2026-06-07

Base principle (locked with Paul): **the daemon owns ALL application logic** —
context, tool curation, tool execution, gates, approval, memory, hooks, behavior,
multi-agent. **Channels / background / voice are pure adapters** (protocol/media
translation, zero logic). Voie B must respect this: the realtime *brain* lives in
the daemon's world, NOT in the voice adapter.

This corrects the earlier (wrong) sketch where the voice process owned the OpenAI
Realtime session + a tool bridge — that put a brain inside an adapter.

---

## 0. The key idea — control / media split

A speech-to-speech model is a duplex *audio* stream, but the *decisions* it makes
(which tool, what to say) are *logic*. Digitorn's rule ("all logic through the
daemon") + the cardinal rule ("never slow the daemon loop") resolve cleanly if we
split the two planes:

```
            MEDIA plane (audio bytes, hot, never touches the daemon core)
  ┌───────────────────────────────────────────────────────────────────┐
  │ Asterisk/Twilio/WebRTC ⇄ voice-adapter ⇄ realtime-worker ⇄ provider │
  └───────────────────────────────────────────────────────────────────┘
                                      │
                       CONTROL plane (logic, through the daemon)
                                      ▼
                         ┌──────────────────────────────┐
                         │  DAEMON  (the one brain)      │
                         │  • build curated toolset      │
                         │  • function_call → SG-4 gates │
                         │    → middleware → execute      │
                         │  • approval (SG-5)            │
                         │  • transcripts → session store│
                         │  • memory / context / instr.  │
                         └──────────────────────────────┘
```

- **All LOGIC goes through the daemon** (tools, gates, approval, context, memory) —
  honouring "tout passe par le daemon".
- **Audio bytes do NOT touch the daemon core** — they flow voice-adapter ⇄
  realtime-worker ⇄ provider — honouring "never slow/block the daemon loop".

The realtime-worker is the media plane (like the LLM worker is for chat, the
embeddings worker for vectors). The daemon stays the control plane = the brain.

---

## 1. Components

### 1.1 Realtime worker (`digitorn-worker-realtime`) — MEDIA plane
- Holds two WS connections per call: the **voice-adapter audio WS** (caller audio
  in/out) and the **provider realtime WS** (OpenAI Realtime first).
- Pumps caller audio → provider; provider audio → caller. Pure media relay.
- Surfaces **control events** to the daemon over a streaming gRPC (the existing
  worker framework): `session.start`, `function_call`, `user_transcript`,
  `assistant_transcript`, `response.done`, `error`.
- Receives **control commands** from the daemon: `configure(tools, instructions,
  audio_format)`, `function_result(call_id, output)`, `cancel`, `close`.
- Provider-agnostic seam: OpenAI Realtime now; Gemini Live / Ultravox later.

### 1.2 Daemon realtime control — CONTROL plane (the brain)
A new runtime mode beside the text turn loop. Per call:
1. On `session.start`: build the curated **toolset** (BuildAgentToolset / SG-3),
   translate to the provider's function format, assemble **instructions** (context
   builder + memory working-set, spoken-style), send `configure` to the worker.
2. On `function_call`: run it through the **SAME chokepoint as a text turn** —
   RunGates (SG-4), middleware onion, workdir policy, approval (SG-5). Persist the
   call + result to the session store. Reply `function_result` to the worker.
3. On `user_transcript` / `assistant_transcript`: append to the session store
   (durable history + memory), exactly like a text turn's messages.
4. On `response.done` / `error`: lifecycle events on the bus (Socket.IO), like a turn.

**Reuse, not reinvention** — the realtime brain calls the exact same tool dispatch,
gates, middleware, approval, session store, memory the text engine uses. Tools are
gated identically whether the brain is a chat LLM or a realtime model.

### 1.3 Voice adapter (`digitorn-voice`) — call-side media bridge
- In realtime mode it does **no STT/TTS, no logic**: it bridges the call's audio
  (AudioSocket / Twilio / WebRTC) to the realtime-worker's audio WS. The thinnest
  possible adapter.
- (Voie A — pipeline — keeps STT/TTS in the adapter; that path is unchanged.)

---

## 2. Tool-call interception — the core sequence

```
provider model decides → emits function_call(name, args, call_id)
  → realtime-worker forwards function_call to daemon (gRPC control)
     → daemon: RunGates(SG-4) + middleware + workdir + (approval SG-5 if required)
        → daemon executes the tool (same dispatch as a text turn)
        → daemon persists call+result to session store
     → daemon sends function_result(call_id, output) to worker
  → worker → provider conversation.item(function_call_output) + response.create
→ model resumes speaking with the result
```

The model PROPOSES; the daemon AUTHORISES + EXECUTES. Identical trust model to text.

---

## 3. Routing — LLM via gateway

Per the standing principle. Two phases:
- **Now**: realtime-worker connects **direct** to the provider (BYOK) via a seam.
  The daemon still owns all logic.
- **Later**: the gateway grows a **realtime WS proxy** (gateway-go, Paul's domain);
  the worker points at the gateway instead of the provider. Config, not redesign.

---

## 4. Approval (SG-5) in a voice call
A tool needing approval can't pop a dialog mid-call. V1 options, per-app config:
- `auto_deny` (safest default) — the gate denies, the model is told "not permitted".
- `speak_and_wait` — the agent says "I need approval for X", the daemon raises the
  normal SG-5 approval event (answered from the web/CLI), the call holds.
- `supervisor` — route to a human operator (future).
Default `auto_deny`; opt into the others.

---

## 5. Isolation, security, scale
- **Isolation**: audio never touches the daemon core (media plane = worker). The
  daemon ⇄ worker link carries only control (small JSON-ish events). Worker crash
  drops the call, never the daemon.
- **Security**: every tool call gated by SG-4 in the daemon — no realtime bypass.
  Provider keys live in the worker (resolved like channel secrets), never logged.
- **Scale**: one realtime-worker process can hold N calls (bounded); spawn more
  workers past the cap. The daemon sees only control traffic → its loop stays cold.

---

## 6. Open decisions (for Paul)
1. **Media placement** — realtime-worker (audio bypasses daemon core, control to
   daemon) vs audio literally through the daemon. *Recommend worker* (protects the
   cardinal rule; consistent with LLM/embeddings workers).
2. **Provider connection** — direct-from-worker (BYOK) now with a gateway-proxy seam
   for later, vs wait for the gateway realtime proxy first. *Recommend direct-now +
   seam* (don't block B on gateway-go work).
3. **First provider** — OpenAI Realtime (confirmed earlier).

---

## 7. Phased plan
1. **Design validation** (this doc).
2. **Daemon realtime control core** — RealtimeSession in the runtime: toolset build
   + function_call → gated dispatch + transcript persistence. Unit-tested with a
   FAKE worker/provider (no keys).
3. **Realtime worker skeleton** — gRPC control stream + audio WS seam; fake provider.
4. **OpenAI Realtime provider adapter** — the real WS protocol, behind the seam.
5. **Voice adapter realtime pipe** — bridge call audio ⇄ worker audio WS.
6. **Gateway realtime proxy** (Paul, later) — route via gateway.
7. **Live test together.**

Each phase: tested + isolated; the daemon stays the one brain.
