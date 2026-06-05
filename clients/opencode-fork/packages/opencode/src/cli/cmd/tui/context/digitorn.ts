// Digitorn adapter — the bridge that lets opencode's STOCK TUI run on OUR Go
// daemon instead of opencode's backend. It lives entirely here, in the fork's
// client layer : NO separate proxy process, NO daemon changes. sdk.tsx swaps
// opencode's createOpencodeClient + SSE for this when DIGITORN_URL is set.
//
// This file is the protocol-grounded FOUNDATION : config/auth + the REST helper
// + the Socket.IO event bridge to our daemon. The opencode-client method surface
// and the Envelope→GlobalEvent translation are layered on top of it.
import { readFileSync, appendFileSync } from "node:fs"
import { homedir } from "node:os"
import { join } from "node:path"
import { io, type Socket } from "socket.io-client"

// Opt-in file tracing : set DIGITORN_DEBUG=<path> to capture the event chain
// (SSE open, socket connect/join, raw envelopes, fan-out) inside the live TUI,
// which can't be observed any other way. No-op when unset.
const DBG = process.env.DIGITORN_DEBUG
const dlog = (msg: string): void => {
  if (!DBG) return
  try {
    appendFileSync(DBG, `${new Date().toISOString()} ${msg}\n`)
  } catch {}
}

export interface DigitornConfig {
  url: string // daemon base, e.g. http://localhost:8000
  token: string // JWT bearer
  userID: string // sub / user id (socket handshake + REST)
  app: string // which digitorn app the TUI session binds to
}

// Our daemon's wire shape on the `/events` namespace (mirrors SocketEnvelope —
// see clients/cli/internal/client/realtime.go). type drives the translation.
export interface DaemonEnvelope {
  type: string
  seq?: number
  session_id?: string
  app_id?: string
  payload?: Record<string, unknown>
  // a few hot fields the daemon promotes out of payload
  live_output_tokens?: number
  [k: string]: unknown
}

// digitornConfig resolves the daemon connection from env first, then the cached
// CLI credentials (~/.digitorn/credentials.json) for token + user.
export function digitornConfig(): DigitornConfig {
  let token = process.env.DIGITORN_TOKEN ?? ""
  let userID = process.env.DIGITORN_USER ?? ""
  if (!token || !userID) {
    try {
      const raw = readFileSync(join(homedir(), ".digitorn", "credentials.json"), "utf8")
      const c = JSON.parse(raw) as Record<string, string>
      token ||= c.access_token ?? c.token ?? c.jwt ?? ""
      userID ||= c.sub ?? c.user_id ?? c.userID ?? ""
    } catch {
      // no cached creds — env must supply them
    }
  }
  return {
    url: (process.env.DIGITORN_URL ?? "http://localhost:8000").replace(/\/+$/, ""),
    token,
    userID,
    app: process.env.DIGITORN_APP ?? "chat-claude",
  }
}

// daemonFetch is the REST helper : Bearer auth, JSON in/out, throws on non-2xx
// with the daemon's error body so callers see real failures.
export async function daemonFetch<T = unknown>(
  cfg: DigitornConfig,
  path: string,
  init: RequestInit = {},
): Promise<T> {
  const res = await fetch(cfg.url + path, {
    ...init,
    headers: {
      authorization: `Bearer ${cfg.token}`,
      "content-type": "application/json",
      ...(init.headers ?? {}),
    },
  })
  if (!res.ok) {
    const body = await res.text().catch(() => "")
    throw new Error(`digitorn ${init.method ?? "GET"} ${path} → ${res.status} ${body}`)
  }
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

// ---------------------------------------------------------------------------
// ÉTAPE 0 — the skeleton fetch. opencode's TUI talks to its server through a
// single `fetch`. We give it ONE that answers opencode's routes : boot routes
// get valid STUBS (so the UI lights up empty without crashing), and /event is a
// long-lived SSE stream. Feature by feature, each route below is then wired to
// our real daemon. The TUI + its reactive client run UNCHANGED on top.
// ---------------------------------------------------------------------------

const jsonRes = (body: unknown, status = 200): Response =>
  new Response(status === 204 ? null : JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  })

const toMs = (iso?: string): number => {
  const t = iso ? Date.parse(iso) : NaN
  return Number.isFinite(t) ? t : Date.now()
}

// Zero-pad a sequence into a fixed-width, lexicographically-sortable id segment.
// opencode orders messages by id and parts by id (sync.tsx Binary.search), so
// our synthetic ids must sort in arrival order — padding makes numeric == lexical.
const padNum = (n: number, w = 12): string =>
  String(Math.max(0, Math.trunc(Number(n) || 0))).padStart(w, "0")

// Daemon tool names join module + tool with DIFFERENT separators depending on
// the app's injection mode: "filesystem.read" (dot), "filesystem__read" (double
// underscore), "filesystem_read" / "bash_run" / "context_builder_run_parallel"
// (single underscore — ambiguous because tool names themselves contain "_").
// So we normalise every separator to "_" and match by KNOWN tool suffix
// (longest/most-specific first) → the bare verb, regardless of form.
const KNOWN_TOOLS = [
  "run_parallel",
  "multiedit",
  "read",
  "write",
  "edit",
  "glob",
  "grep",
  "search",
  "fetch",
  "bash",
  "shell",
  "exec",
  "run",
  "ls",
  "list",
]
const cleanToolName = (n: unknown): string => {
  const norm = String(n || "tool")
    .toLowerCase()
    .replace(/__/g, "_")
    .replace(/\./g, "_")
  for (const t of KNOWN_TOOLS) {
    if (norm === t || norm.endsWith("_" + t)) return t
  }
  return norm
}

// Internal bookkeeping tools (memory/goal/todos) — never shown as a tool chip,
// exactly like the old CLI's isHiddenTool. Their effects surface elsewhere
// (todos via the todo widget, goal/memory silently). Matched by suffix.
const HIDDEN_TOOLS = ["set_goal", "remember", "task_create", "task_update"]
const isHiddenTool = (raw: unknown): boolean => {
  const norm = String(raw ?? "").toLowerCase().replace(/__/g, "_").replace(/\./g, "_")
  return HIDDEN_TOOLS.some((t) => norm === t || norm.endsWith("_" + t))
}

// Sub-agent delegation (agent_spawn.agent / agent_spawn__agent) → opencode's
// "task" tool. The model launches sub-agents two ways : as a DIRECT tool_call,
// or batched inside context_builder.run_parallel (tasks=[agent_spawn ×N]). Both
// map to the SAME task part, so the detection + input shaping is shared here.
const isAgentSpawn = (raw: unknown): boolean => /agent_spawn/i.test(String(raw ?? ""))
// opencode's Task renderer shows a SHORT one-line description + the sub-agent
// type. Our spawn arg `task` is the full multi-paragraph prompt, so prefer the
// one-line `memory_seed` ("Goal: …"), else the first non-empty prompt line,
// capped — never dump the whole prompt.
const spawnInput = (args: unknown): Record<string, unknown> => {
  const a = (args ?? {}) as Record<string, any>
  const full = String(a.task ?? a.prompt ?? "")
  const seed = String(a.memory_seed ?? "").replace(/^\s*goal\s*:\s*/i, "").trim()
  const short = (seed || full.split("\n").map((s) => s.trim()).find(Boolean) || "Subagent").trim()
  return {
    description: short.length > 80 ? short.slice(0, 79) + "…" : short,
    subagent_type: String(a.agent ?? a.kind ?? "general"),
  }
}

// friendlyError maps raw daemon failure text to actionable wording — ported from
// the old CLI's friendlyError (the gateway-401 / timeout / unreachable cases get
// a hint the user can act on). Everything else passes through unchanged.
const friendlyError = (reason: string): string => {
  const low = reason.toLowerCase()
  if (low.includes("401") && (low.includes("provider") || low.includes("gateway") || low.includes("bifrost")))
    return "gateway rejected your token (401) — run `digitorn login` to renew"
  if (low.includes("context deadline exceeded") || low.includes("timeout"))
    return "LLM call timed out — try again, or check the gateway"
  if (low.includes("connection refused") || low.includes("no such host"))
    return "couldn't reach the LLM gateway — is it running?"
  return reason
}
// errorInfo turns OUR daemon `error` event payload (the legacy DaemonError shape
// {error,message,code,category,detail,retry}; see ErrorPayload / emitTurnError)
// into opencode's message.error shape {name,data:{message}}. opencode paints
// data.message in a red bordered box; `name` only matters to flag an abort
// (MessageAbortedError → "interrupted"), set elsewhere.
const errorInfo = (p: any): { name: string; data: { message: string } } => {
  const base = String(p?.error || p?.message || p?.detail || "").trim() || "the assistant turn failed"
  const detail = String(p?.detail || "").trim()
  let message = friendlyError(base)
  if (detail && detail !== base && !message.includes(detail)) message += `\n${detail}`
  return { name: String(p?.code || p?.category || "DaemonError"), data: { message } }
}

// opencode renders tool parts with per-tool components keyed by part.tool, each
// reading specific input fields. We translate OUR tool name + args INTO opencode's
// exact shapes so its native renderers light up (anti-corruption layer). Unknown
// tools keep their clean name and fall to opencode's GenericTool.
const TOOL_ALIASES: Record<string, string> = {
  glob: "glob",
  grep: "grep",
  read: "read",
  write: "write",
  edit: "edit",
  multiedit: "edit",
  run: "bash",
  bash: "bash",
  shell: "bash",
  exec: "bash",
  search: "websearch",
  web_search: "websearch",
  fetch: "webfetch",
  web_fetch: "webfetch",
}
const mapTool = (rawName: unknown, args: Record<string, unknown> = {}): { tool: string; input: Record<string, unknown> } => {
  const tool = TOOL_ALIASES[cleanToolName(rawName)] ?? cleanToolName(rawName)
  const a = (args ?? {}) as Record<string, any>
  // Mapped tools : feed ONLY the field(s) the opencode renderer reads, with its
  // exact names. The renderers append input() of the LEFTOVER params, so any
  // extra (our `path` duplicate, timeout_seconds, …) would show as ugly [k=v].
  const out: Record<string, unknown> = {}
  const set = (k: string, v: unknown) => {
    if (v != null) out[k] = v
  }
  const fp = a.filePath ?? a.path ?? a.file_path
  switch (tool) {
    case "read":
    case "edit":
      set("filePath", fp)
      break
    case "write":
      set("filePath", fp)
      set("content", a.content)
      break
    case "glob":
    case "grep":
      set("pattern", a.pattern)
      set("path", a.path)
      break
    case "bash":
      set("command", a.command)
      break
    case "websearch":
      set("query", a.query)
      break
    case "webfetch":
      set("url", a.url)
      break
    default:
      return { tool, input: { ...a } } // unknown tool → opencode's GenericTool
  }
  return { tool, input: out }
}

// Result metadata opencode's renderers read to show a COMPLETED tool's outcome
// ("(N matches)", loaded file, …) — what makes a finished tool visually distinct
// from a running one (beyond the spinner). Parsed from the daemon's output.
const toolMetadata = (tool: string, output: string, _input: Record<string, unknown>): Record<string, unknown> => {
  // glob/grep: a match count → "(N matches)". (NOT read's `loaded` — that field
  // is for EXTRA files and just duplicates the "Read <file>" line.)
  if (tool === "glob" || tool === "grep") {
    try {
      const j = JSON.parse(output)
      const n = typeof j?.count === "number" ? j.count : Array.isArray(j?.files) ? j.files.length : undefined
      if (typeof n === "number") return tool === "grep" ? { matches: n } : { count: n }
    } catch {}
  }
  return {}
}

// The primary param of a tool (file/pattern/command/…), used as the ToolPart
// `state.title`. opencode's Task renderer shows "↳ <Tool> <title>" for a
// sub-agent's current tool, so this is what makes the nested line meaningful.
const primaryParam = (input: Record<string, unknown>): string => {
  const i = (input ?? {}) as Record<string, any>
  let v = i.filePath ?? i.pattern ?? i.command ?? i.query ?? i.url ?? i.path
  if (v == null) v = Object.values(i).find((x) => typeof x === "string" || typeof x === "number")
  const s = v == null ? "" : String(v).replace(/\s+/g, " ")
  return s.length > 50 ? s.slice(0, 49) + "…" : s
}

// toOcSession translates OUR daemon's session (ListSessions shape — see the Go
// CLI internal/client/types.go Session, the reference) into opencode's Session.
function toOcSession(s: Record<string, any>, dir: string): Record<string, unknown> {
  const id = String(s.session_id ?? s.id ?? "")
  return {
    id,
    slug: id,
    projectID: "digitorn",
    directory: s.workdir || dir,
    title: s.title || s.preview || "Session",
    version: "0.0.0",
    time: { created: toMs(s.started_at), updated: toMs(s.updated_at || s.started_at) },
  }
}

// digitornFetch returns a fetch implementation routing opencode's API surface.
export function digitornFetch(cfg: DigitornConfig): typeof fetch {
  const dir = process.cwd()
  const now = Date.now()

  // ONE synthetic "connected" provider so opencode lets the user send prompts.
  // The real model is chosen server-side by the digitorn app, so this is just a
  // valid placeholder that satisfies useConnected() (id !== "opencode") and gives
  // the UI a current model. Shapes mirror the v2 SDK Provider/Model exactly.
  const dgModel = {
    id: "build",
    providerID: "digitorn",
    api: { id: "build", url: cfg.url, npm: "" },
    name: "Digitorn",
    capabilities: {
      temperature: true,
      reasoning: true,
      attachment: false,
      toolcall: true,
      input: { text: true, audio: false, image: false, video: false, pdf: false },
      output: { text: true, audio: false, image: false, video: false, pdf: false },
      interleaved: false,
    },
    cost: { input: 0, output: 0, cache: { read: 0, write: 0 } },
    limit: { context: 200000, output: 8192 },
    status: "active",
    options: {},
    headers: {},
    release_date: "2026-01-01",
  }
  const dgProvider = {
    id: "digitorn",
    name: "Digitorn",
    source: "custom",
    env: [] as string[],
    options: {},
    models: { build: dgModel },
  }
  const dgDefault = { digitorn: "build" }

  // Digitorn is a MULTI-APP platform (chat-claude, claude-code, web-probe, …);
  // currentApp is the live selection, switchable at runtime via /digitorn/app.
  let currentApp = cfg.app

  // The /api/apps LIST omits icon/color (only the per-app detail has them), so we
  // enrich once and cache (apps don't change at runtime).
  let appsCache: Array<{ id: string; name: string; description: string; category: string; icon: string; color: string }> | undefined
  const loadApps = async () => {
    if (appsCache) return appsCache
    const r = await daemonFetch<any>(cfg, `/api/apps`)
    const list: Record<string, any>[] = (Array.isArray(r) ? r : (r?.apps ?? [])).filter((a: any) => a.enabled !== false)
    appsCache = await Promise.all(
      list.map(async (a) => {
        const id = String(a.app_id ?? a.id ?? "")
        let icon = a.icon
        let color = a.color
        if (icon == null || color == null) {
          try {
            const d = await daemonFetch<any>(cfg, `/api/apps/${encodeURIComponent(id)}`)
            icon = icon ?? d?.icon
            color = color ?? d?.color
          } catch {}
        }
        return { id, name: String(a.name ?? id), description: String(a.description ?? ""), category: String(a.category ?? ""), icon: String(icon ?? ""), color: String(color ?? "") }
      }),
    )
    return appsCache
  }

  // An app's composer MODES (runtime.modes : build, plan, …) — our equivalent of
  // opencode "agents". The old CLI cycled these with Shift+Tab and sent the
  // active one with each message. Pulled from the compiled manifest, cached per app.
  const modesCache = new Map<string, Array<{ id: string; label: string; description: string }>>()
  const loadModes = async (appId: string) => {
    const hit = modesCache.get(appId)
    if (hit) return hit
    let modes: Array<{ id: string; label: string; description: string }> = []
    try {
      const m = await daemonFetch<any>(cfg, `/api/apps/${encodeURIComponent(appId)}/manifest`)
      const rt = m?.runtime ?? m?.Runtime ?? {}
      const defs = rt?.modes ?? rt?.Modes ?? {}
      const order: string[] = (rt?.modes_order ?? rt?.ModesOrder ?? Object.keys(defs)) as string[]
      modes = order
        .filter((id) => defs[id])
        .map((id) => {
          const d = defs[id] ?? {}
          return { id, label: String(d.label ?? d.Label ?? id), description: String(d.description ?? d.Description ?? "") }
        })
    } catch {}
    modesCache.set(appId, modes)
    return modes
  }

  // ── ÉTAPE 3 : live chat plumbing ──────────────────────────────────────────
  // ONE Socket.IO to our daemon, fanned out to every open /event SSE stream,
  // translating each Envelope into opencode GlobalEvents. Per-session turn state
  // builds opencode "parts" from our streaming deltas.
  const sinks = new Set<(ev: unknown) => void>()
  const joined = new Set<string>()
  let socket: Socket | undefined
  // Connection state for the footer dot, modelled on the Go CLI's socket-driven
  // Realtime: connecting (startup) → connected → disconnected (daemon dropped).
  // Driven by the /events socket events and broadcast (not polled) to the footer.
  let connState: "connecting" | "connected" | "reconnecting" | "disconnected" = "connecting"
  // Live context occupancy + cumulative spend per session, fed from the daemon's
  // context_tokens (total/window) and cost_update/token_usage (usd_total) events
  // — the SAME authoritative numbers the Go CLI footer gauge reads. Broadcast to
  // the sidebar Context panel via the pushed digitorn.context event (the panel
  // reads a one-shot snapshot on mount, then lives off the push); kept here too
  // so a freshly-opened event stream can be replayed the current state.
  const ctxBySession = new Map<string, { used: number; window: number }>()
  const ctxFor = (sid: string) => {
    let v = ctxBySession.get(sid)
    if (!v) {
      v = { used: 0, window: 0 }
      ctxBySession.set(sid, v)
    }
    return v
  }
  // Once we've connected at least once, a later connect_error means the daemon
  // dropped (stay red), vs the initial pre-connect attempts (yellow connecting).
  let everConnected = false
  // Pending tool-call gates : requestID → sessionID, so the permission reply
  // route (which only carries requestID) can address our daemon's /approve.
  const pendingApprovals = new Map<string, string>()
  // requestIDs that are ask_user questions (kind=="question"), so we resolve them
  // as opencode question.* events instead of permission.* on granted/denied.
  const questionReqs = new Set<string>()
  // Per-session todo list (insertion-ordered). Our daemon emits single-todo
  // deltas (todo_added/updated); opencode's todo.updated wants the FULL list, so
  // we accumulate here and re-emit the whole set on every change.
  const todoLists = new Map<string, Map<string, { text: string; status: string }>>()
  // run_parallel runs N sub-tools at once. We DON'T show a wrapper — we expand it
  // into one native ToolPart per child (Read/Glob/Bash…), so it reads as the tools
  // executing in parallel. callID → the children, to fill each output on the result.
  type ParChild = { partID: string; callID: string; name: string; input: Record<string, unknown> }
  const parallelChildren = new Map<string, ParChild[]>()

  // Per-session turn state. ONE assistant message per turn holds an ORDERED set
  // of parts (reasoning → text → tool → text …). Parts render in lexicographic
  // part.id order (sync.tsx Binary.search), so every id carries a zero-padded
  // ordinal that follows arrival. Text/reasoning text accumulates per part id.
  type ToolRec = { partID: string; name: string; input: Record<string, unknown>; start: number; sessionId?: string }
  type Turn = {
    msgID: string
    created: number
    textPartID?: string
    reasoningPartID?: string
    reasoningStart?: number // wall-clock start of the CURRENT thinking round (per-block duration)
    error?: { name: string; data: { message: string } } // sticky once set → survives turn_ended re-emits
    buf: Map<string, string>
    tools: Map<string, ToolRec>
    streamed?: boolean // any assistant_delta seen → deltas own the text
  }
  const turns = new Map<string, Turn>()
  // Find a tool rec by its callID across turns (agent_spawn events may land in a
  // different session than the delegation tool_call).
  const findTool = (callID: string): { turn: Turn; sid: string; rec: ToolRec } | undefined => {
    for (const [sid, turn] of turns) {
      const rec = turn.tools.get(callID)
      if (rec) return { turn, sid, rec }
    }
    return undefined
  }

  const partsText = (p: any): string =>
    Array.isArray(p?.parts) ? p.parts.map((x: any) => x?.text ?? "").join("") : String(p?.content ?? p?.text ?? "")
  // directory:"global" makes every event pass useEvent()'s router filter
  // (event.ts: `directory === "global" || project === current`). We have one
  // project, so global routing delivers every event to the reducer.
  const wrap = (payload: unknown) => ({ directory: "global", payload })
  const emit = (ev: unknown) => {
    if (DBG) dlog(`emit→${sinks.size} sinks : ${JSON.stringify((ev as any)?.payload?.type)}`)
    for (const s of sinks) s(ev)
  }
  const partUpdated = (sid: string, part: Record<string, unknown>) =>
    emit(wrap({ type: "message.part.updated", properties: { sessionID: sid, part } }))
  // PUSH (not poll) — the sidebar connection dot + context gauge are driven by
  // events through the SAME stream the chat uses, exactly like the Go CLI's
  // socket-driven footer. broadcastConn fires on every socket transition;
  // broadcastCtx fires when the daemon recounts occupancy / cost.
  const connProps = () => ({ state: connState })
  const broadcastConn = () => emit(wrap({ type: "digitorn.connection", properties: connProps() }))
  const ctxProps = (sid: string) => {
    const c = ctxFor(sid)
    const percent = c.window > 0 ? Math.round((c.used / c.window) * 100) : 0
    return { session: sid, tokens: c.used, window: c.window, percent }
  }
  const broadcastCtx = (sid: string) => emit(wrap({ type: "digitorn.context", properties: ctxProps(sid) }))
  const todosFor = (sid: string) => {
    const m = todoLists.get(sid)
    return m ? [...m.values()].map((t) => ({ content: t.text, status: t.status || "pending", priority: "medium" })) : []
  }
  const emitTodos = (sid: string) => emit(wrap({ type: "todo.updated", properties: { sessionID: sid, todos: todosFor(sid) } }))
  const asstInfo = (
    sid: string,
    id: string,
    created: number,
    completed?: number,
    error?: { name: string; data: { message: string } },
  ) => ({
    id,
    sessionID: sid,
    role: "assistant",
    time: completed ? { created, completed } : { created },
    parentID: "",
    modelID: "build",
    providerID: "digitorn",
    mode: "build",
    agent: "build",
    path: { cwd: dir, root: dir },
    cost: 0,
    tokens: { input: 0, output: 0, reasoning: 0, cache: { read: 0, write: 0 } },
    ...(error ? { error } : {}),
  })
  const ensureTurn = (sid: string, seq: number): Turn => {
    let cur = turns.get(sid)
    if (!cur) {
      const created = Date.now()
      cur = { msgID: `${sid}:m:${padNum(seq)}`, created, buf: new Map(), tools: new Map() }
      turns.set(sid, cur)
      emit(wrap({ type: "message.updated", properties: { sessionID: sid, info: asstInfo(sid, cur.msgID, created) } }))
    }
    return cur
  }
  // Part id carries the daemon seq of the event that CREATED the part, so parts
  // sort within a message by the daemon's authoritative sequence (each event has
  // a unique seq, and one event creates at most one part — run_parallel children,
  // born from a single event, get a sub-index off the header id).
  const nextPartID = (cur: Turn, seq: number) => `${cur.msgID}:p${padNum(seq)}`
  // Emit a sub-agent "task" part — but ONLY once it has a description (opencode's
  // Task renderer shows nothing without one, so emitting during the streaming of
  // the call's args would just be a bare spinner).
  const emitTask = (sid: string, msgID: string, callID: string, rec: ToolRec) => {
    if (!rec.input.description) return
    partUpdated(sid, {
      id: rec.partID,
      sessionID: sid,
      messageID: msgID,
      type: "tool",
      callID,
      tool: "task",
      state: { status: "running", input: rec.input, ...(rec.sessionId ? { metadata: { sessionId: rec.sessionId } } : {}), time: { start: rec.start } },
    })
  }
  // Close the open reasoning block : stamp time.end (opencode shows "thinking…"
  // until end is set) and clear the id so a later thinking round opens a NEW
  // part instead of growing one block for the whole turn.
  const finalizeReasoning = (sid: string, cur: Turn) => {
    const id = cur.reasoningPartID
    if (!id) return
    const start = cur.reasoningStart ?? cur.created
    cur.reasoningPartID = undefined
    cur.reasoningStart = undefined
    partUpdated(sid, { id, sessionID: sid, messageID: cur.msgID, type: "reasoning", text: cur.buf.get(id) ?? "", time: { start, end: Date.now() } })
  }

  const translate = (env: DaemonEnvelope) => {
    const sid = env.session_id ?? ""
    if (!sid) return
    const p: any = env.payload ?? {}
    const seq = typeof env.seq === "number" ? env.seq : Date.now()
    const t = Date.now()
    switch (env.type) {
      case "user_message": {
        const id = `${sid}:m:${padNum(seq)}`
        emit(
          wrap({
            type: "message.updated",
            properties: {
              sessionID: sid,
              info: { id, sessionID: sid, role: "user", time: { created: t }, agent: "build", model: { providerID: "digitorn", modelID: "build" } },
            },
          }),
        )
        partUpdated(sid, { id: `${id}:p${padNum(0, 4)}`, sessionID: sid, messageID: id, type: "text", text: partsText(p) })
        break
      }
      case "assistant_delta": {
        const cur = ensureTurn(sid, seq)
        finalizeReasoning(sid, cur) // thinking → answer : close the thinking block
        cur.streamed = true
        if (!cur.textPartID) cur.textPartID = nextPartID(cur, seq)
        const id = cur.textPartID
        const text = (cur.buf.get(id) ?? "") + partsText(p)
        cur.buf.set(id, text)
        partUpdated(sid, { id, sessionID: sid, messageID: cur.msgID, type: "text", text })
        break
      }
      case "assistant_reasoning_delta": {
        // Our daemon streams thinking in the flat `reasoning` field (not parts[]).
        const delta = String(p.reasoning ?? "")
        if (!delta) break
        const cur = ensureTurn(sid, seq)
        if (!cur.reasoningPartID) {
          cur.reasoningPartID = nextPartID(cur, seq)
          cur.reasoningStart = Date.now() // this round's own start → real per-block duration
          cur.textPartID = undefined // text AFTER this round opens a NEW part below it (not merged above)
        }
        const id = cur.reasoningPartID
        const text = (cur.buf.get(id) ?? "") + delta
        cur.buf.set(id, text)
        partUpdated(sid, { id, sessionID: sid, messageID: cur.msgID, type: "reasoning", text, time: { start: cur.reasoningStart ?? cur.created } })
        break
      }
      case "tool_call": {
        const callID = String(p.call_id ?? "")
        if (!callID || isHiddenTool(p.name)) break // bookkeeping tools never show
        const cur = ensureTurn(sid, seq)
        finalizeReasoning(sid, cur) // thinking → tool : close the thinking block
        cur.textPartID = undefined // any post-tool text opens a NEW part, after the tool
        const hasArgs = p.arguments && typeof p.arguments === "object"

        // run_parallel → expand into individual native child ToolParts (no
        // wrapper/header), so the parallel tools render exactly like normal ones.
        if (cleanToolName(p.name) === "run_parallel") {
          const tasks = hasArgs && Array.isArray((p.arguments as any).tasks) ? (p.arguments as any).tasks : []
          if (tasks.length && !parallelChildren.has(callID)) {
            const base = `${cur.msgID}:p${padNum(seq)}` // children sub-index off this seq → ordered, consecutive
            parallelChildren.set(
              callID,
              tasks.map((task: any, i: number) => {
                const childCall = `${callID}:${i}`
                const partID = `${base}.${padNum(i, 3)}`
                // A sub-agent spawned inside run_parallel → render as a "task" part
                // (opencode's sub-agent renderer), not a generic tool. Its agent_spawn
                // EVENT binds the child session via parent_call_id = `${callID}:${i}`,
                // and findTool looks in cur.tools — so register the rec THERE (too) so
                // the binding lands on this exact part and its nested activity shows.
                if (isAgentSpawn(task?.tool)) {
                  const input = spawnInput(task?.args)
                  const rec: ToolRec = { partID, name: "task", input, start: t }
                  cur.tools.set(childCall, rec)
                  emitTask(sid, cur.msgID, childCall, rec)
                  return { partID, callID: childCall, name: "task", input }
                }
                const m = mapTool(task?.tool, task?.args && typeof task.args === "object" ? task.args : {})
                partUpdated(sid, { id: partID, sessionID: sid, messageID: cur.msgID, type: "tool", callID: childCall, tool: m.tool, state: { status: "running", input: m.input, title: primaryParam(m.input), time: { start: t } } })
                return { partID, callID: childCall, name: m.tool, input: m.input }
              }),
            )
          }
          break
        }

        // Sub-agent delegation as a DIRECT tool_call (agent_spawn.agent) → opencode's
        // "task" tool. The child session id arrives later via the agent_spawn event
        // (→ metadata.sessionId), which binds it by this call_id.
        if (isAgentSpawn(p.name)) {
          let rec = cur.tools.get(callID)
          if (!rec) {
            rec = { partID: nextPartID(cur, seq), name: "task", input: {}, start: t }
            cur.tools.set(callID, rec)
          }
          const a = (hasArgs ? p.arguments : {}) as any
          if (a.task || a.prompt || a.memory_seed) rec.input = spawnInput(a)
          emitTask(sid, cur.msgID, callID, rec) // only renders once a description exists
          break
        }

        const mapped = mapTool(p.name, hasArgs ? (p.arguments as Record<string, unknown>) : {})
        let rec = cur.tools.get(callID)
        if (!rec) {
          rec = { partID: nextPartID(cur, seq), name: mapped.tool, input: {}, start: t }
          cur.tools.set(callID, rec)
        }
        rec.name = mapped.tool
        if (hasArgs) rec.input = mapped.input
        partUpdated(sid, {
          id: rec.partID,
          sessionID: sid,
          messageID: cur.msgID,
          type: "tool",
          callID,
          tool: rec.name,
          state: { status: "running", input: rec.input, title: primaryParam(rec.input), time: { start: rec.start } },
        })
        break
      }
      case "tool_result": {
        const cur = turns.get(sid)
        if (!cur || isHiddenTool(p.name)) break
        const callID = String(p.call_id ?? "")

        // run_parallel result : {results:[{content,status}]} aligned to task order
        // → settle each expanded child to completed/error with its output.
        const kids = parallelChildren.get(callID)
        if (kids) {
          let results: any[] = []
          try {
            const parsed = JSON.parse(partsText(p))
            if (Array.isArray(parsed?.results)) results = parsed.results
          } catch {}
          kids.forEach((kid, i) => {
            const r = results[i] ?? {}
            const out = String(r.content ?? "")
            const ok = String(r.status ?? "completed") === "completed"
            // A sub-agent child : settle as a completed "task" and KEEP the bound
            // sessionId (set on the rec by its agent_spawn event) so opencode's
            // Task renderer still resolves the child session for its toolcall count.
            if (kid.name === "task") {
              const rec = cur.tools.get(kid.callID)
              const sessionId = rec?.sessionId
              partUpdated(sid, {
                id: kid.partID,
                sessionID: sid,
                messageID: cur.msgID,
                type: "tool",
                callID: kid.callID,
                tool: "task",
                state: ok
                  ? { status: "completed", input: kid.input, output: out, ...(sessionId ? { metadata: { sessionId } } : {}), time: { start: rec?.start ?? t, end: t } }
                  : { status: "error", input: kid.input, error: out || "failed", time: { start: rec?.start ?? t, end: t } },
              })
              return
            }
            partUpdated(sid, {
              id: kid.partID,
              sessionID: sid,
              messageID: cur.msgID,
              type: "tool",
              callID: kid.callID,
              tool: kid.name,
              state: ok
                ? { status: "completed", input: kid.input, output: out, title: primaryParam(kid.input) || kid.name, metadata: toolMetadata(kid.name, out, kid.input), time: { start: t, end: t } }
                : { status: "error", input: kid.input, error: out || "failed", time: { start: t, end: t } },
            })
          })
          parallelChildren.delete(callID)
          break
        }

        let rec = cur.tools.get(callID)
        if (!rec) {
          rec = { partID: nextPartID(cur, seq), name: mapTool(p.name).tool, input: {}, start: t }
          cur.tools.set(callID, rec)
        }
        const output = partsText(p)
        const dur = Number(p.duration_ms ?? 0) || 0
        const ok = String(p.status ?? "completed") === "completed"
        const state = ok
          ? { status: "completed", input: rec.input, output, title: primaryParam(rec.input) || rec.name, metadata: { duration_ms: dur, ...(rec.sessionId ? { sessionId: rec.sessionId } : {}), ...toolMetadata(rec.name, output, rec.input) }, time: { start: rec.start, end: rec.start + dur } }
          : { status: "error", input: rec.input, error: output || "tool failed", time: { start: rec.start, end: rec.start + dur } }
        partUpdated(sid, { id: rec.partID, sessionID: sid, messageID: cur.msgID, type: "tool", callID, tool: rec.name, state })
        break
      }
      case "assistant_message": {
        const cur = ensureTurn(sid, seq)
        finalizeReasoning(sid, cur)
        // assistant_delta already streamed this step's text into its part —
        // re-emitting here would DUPLICATE it (a fresh part after a tool_call
        // cleared textPartID). Only materialize when nothing streamed (a
        // non-streaming provider that sends the message whole).
        const text = partsText(p)
        if (text && !cur.streamed) {
          const id = cur.textPartID ?? nextPartID(cur, seq)
          cur.textPartID = id
          cur.buf.set(id, text)
          partUpdated(sid, { id, sessionID: sid, messageID: cur.msgID, type: "text", text })
        }
        // The turn stays alive : late tool_result / post-tool text still arrive.
        break
      }
      case "approval_request": {
        const id = String(p.id ?? "")
        if (!id) break
        pendingApprovals.set(id, sid)
        const cur = turns.get(sid)
        if (p.kind === "question") {
          // ask_user → opencode question.asked. The question text rides in
          // `reason` ; choices/multi/extra live in `payload`.
          questionReqs.add(id)
          const pl: any = p.payload ?? {}
          const choices: string[] = Array.isArray(pl.choices) ? pl.choices.map(String) : []
          const q = String(p.reason ?? pl.content ?? "Question")
          emit(
            wrap({
              type: "question.asked",
              properties: {
                id,
                sessionID: sid,
                questions: [
                  {
                    question: q,
                    header: q.slice(0, 30),
                    options: choices.map((c) => ({ label: c, description: "" })),
                    multiple: Boolean(pl.allow_multiple),
                    custom: choices.length === 0, // no choices → free-text answer
                  },
                ],
                tool: { messageID: cur?.msgID ?? "", callID: String(p.call_id ?? "") },
              },
            }),
          )
          break
        }
        // SG-5 tool-call gate → opencode permission.asked.
        emit(
          wrap({
            type: "permission.asked",
            properties: {
              id,
              sessionID: sid,
              permission: cleanToolName(p.tool_name) || "tool",
              patterns: [],
              metadata: { reason: p.reason ?? "", risk: p.risk_level ?? "", args: p.tool_params ?? {} },
              always: [],
              tool: { messageID: cur?.msgID ?? "", callID: String(p.call_id ?? "") },
            },
          }),
        )
        break
      }
      case "approval_granted":
      case "approval_denied": {
        const id = String(p.id ?? "")
        pendingApprovals.delete(id)
        if (questionReqs.delete(id)) {
          const type = env.type === "approval_granted" ? "question.replied" : "question.rejected"
          emit(wrap({ type, properties: { sessionID: sid, requestID: id, answers: [] } }))
        } else {
          emit(wrap({ type: "permission.replied", properties: { sessionID: sid, requestID: id } }))
        }
        break
      }
      case "todo_added": {
        const tid = String(p.id ?? "")
        if (!tid) break
        let m = todoLists.get(sid)
        if (!m) {
          m = new Map()
          todoLists.set(sid, m)
        }
        m.set(tid, { text: String(p.text ?? ""), status: String(p.status ?? "pending") })
        emitTodos(sid)
        break
      }
      case "todo_updated": {
        const tid = String(p.id ?? "")
        const m = todoLists.get(sid)
        if (!tid || !m) break
        const cur = m.get(tid) ?? { text: "", status: "pending" }
        if (p.text != null) cur.text = String(p.text)
        if (p.status != null) cur.status = String(p.status)
        m.set(tid, cur)
        emitTodos(sid)
        break
      }
      case "agent_spawn": {
        // Bind the spawned child session to its delegation task part, so
        // opencode's Task renderer shows the sub-agent's nested activity.
        //
        // We do NOT join the child's session room : the daemon enforces ONE
        // session room per socket (join_session leaves every other session:*
        // room), so joining each child would evict us from the parent room and
        // from every prior child — only the LAST-joined child would still
        // stream. Instead the daemon already fans every sub-agent event out to
        // its ancestor (parent) room tagged with the child's session_id, so by
        // staying in the parent room we receive ALL children's tool activity
        // and translate() routes each by session_id into its own task.
        const parentCall = String(p.parent_call_id ?? "")
        const childSid = String(p.child_session_id ?? "")
        if (!parentCall || !childSid) break
        const found = findTool(parentCall)
        if (!found) break
        found.rec.sessionId = childSid
        emitTask(found.sid, found.turn.msgID, parentCall, found.rec) // emits if description is set
        break
      }
      // Turn failure → opencode's in-chat error banner. Our daemon emits a
      // type:"error" event (ErrorPayload : error/message/code/category/detail/
      // retry) on the failure path; we hang it on the assistant message's
      // info.error, which opencode renders as a red-bordered box. ensureTurn so
      // a failure with no streamed content still has a message to carry it.
      case "error": {
        const cur = ensureTurn(sid, seq)
        finalizeReasoning(sid, cur)
        cur.error = errorInfo(p)
        emit(wrap({ type: "message.updated", properties: { sessionID: sid, info: asstInfo(sid, cur.msgID, cur.created, t, cur.error) } }))
        break
      }
      // Context gauge feed (CTX-7). context_tokens carries the EXACT window
      // occupancy from the background Context Service. Broadcast to the sidebar
      // Context panel (pushed, not polled). ($ spent omitted — the daemon does
      // not track cost yet.)
      case "context_tokens": {
        const c = ctxFor(sid)
        if (typeof p.total === "number") c.used = p.total
        if (typeof p.window === "number" && p.window > 0) c.window = p.window
        if (DBG) dlog(`context_tokens sid=${sid} used=${c.used} window=${c.window}`)
        broadcastCtx(sid)
        break
      }
      case "turn_ended":
      case "session_idle": {
        const cur = turns.get(sid)
        if (cur) {
          finalizeReasoning(sid, cur)
          // turn_ended carries status ∈ {done, errored, interrupted} (+ reason).
          // An `error` event usually already set cur.error (richer); only fall
          // back to the status here when it didn't. interrupted → opencode's
          // MessageAbortedError ("· interrupted"), not the red error box.
          const status = String(p.status ?? "")
          const reason = String(p.reason ?? "")
          if (!cur.error && status === "interrupted") cur.error = { name: "MessageAbortedError", data: { message: reason || "turn interrupted" } }
          else if (!cur.error && status === "errored") cur.error = errorInfo({ error: reason })
          emit(wrap({ type: "message.updated", properties: { sessionID: sid, info: asstInfo(sid, cur.msgID, cur.created, t, cur.error) } }))
        }
        turns.delete(sid)
        emit(wrap({ type: "session.idle", properties: { sessionID: sid } }))
        break
      }
    }
  }

  const ensureSocket = () => {
    if (socket) return
    dlog(`ensureSocket: connecting to ${cfg.url}/events (user=${cfg.userID.slice(0, 8)} tok=${cfg.token.length})`)
    connState = "connecting"
    broadcastConn()
    socket = io(cfg.url + "/events", { transports: ["websocket"], auth: { token: cfg.token, user_id: cfg.userID } })
    socket.on("connect", () => {
      everConnected = true
      connState = "connected"
      broadcastConn()
      dlog(`socket CONNECTED id=${socket?.id}`)
    })
    socket.on("connect_error", (e: any) => {
      // Before the first successful connect these are just startup retries
      // (yellow). After we've been connected, they mean the daemon is gone (red).
      connState = everConnected ? "disconnected" : "connecting"
      broadcastConn()
      dlog(`socket CONNECT_ERROR ${e?.message ?? e}`)
    })
    socket.on("disconnect", (r: any) => {
      connState = "disconnected"
      broadcastConn()
      dlog(`socket DISCONNECT ${r}`)
    })
    socket.on("event", (env: DaemonEnvelope) => {
      if (DBG) dlog(`recv envelope type=${env.type} sid=${String(env.session_id).slice(0, 8)} seq=${env.seq}`)
      try {
        translate(env)
      } catch (e: any) {
        dlog(`translate THREW ${e?.message ?? e}`)
      }
    })
  }
  const joinSession = (sid: string) => {
    ensureSocket()
    if (joined.has(sid)) return
    joined.add(sid)
    dlog(`joinSession ${sid.slice(0, 8)} (connected=${socket?.connected})`)
    socket!.emit("join_session", { session_id: sid })
  }

  return (async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const req = new Request(input as RequestInfo, init)
    const path = new URL(req.url).pathname
    const method = req.method.toUpperCase()
    if (DBG && path !== "/event") dlog(`fetch ${method} ${path}`)

    // The live event stream : a real SSE body. ÉTAPE 0 emits server.connected and
    // stays open (keep-alive). ÉTAPE 2 wires Socket.IO → GlobalEvent into `send`.
    if (path === "/event" || path === "/global/event") {
      ensureSocket()
      const enc = new TextEncoder()
      let sink: ((ev: unknown) => void) | undefined
      const stream = new ReadableStream<Uint8Array>({
        start(controller) {
          const send = (ev: unknown) => {
            try {
              controller.enqueue(enc.encode(`data: ${JSON.stringify(ev)}\n\n`))
            } catch {}
          }
          send({ directory: "global", payload: { type: "server.connected", properties: {} } })
          sink = send
          sinks.add(send) // translated daemon events fan out here
          dlog(`/event SSE opened (sinks now ${sinks.size})`)
          // Replay current connection + context state to this fresh stream so a
          // component that mounts after the socket connected still gets initial
          // state — pushed, not polled.
          send(wrap({ type: "digitorn.connection", properties: connProps() }))
          for (const sid of ctxBySession.keys()) send(wrap({ type: "digitorn.context", properties: ctxProps(sid) }))
          const ping = setInterval(() => {
            try {
              controller.enqueue(enc.encode(`: keepalive\n\n`))
            } catch {}
          }, 15000)
          req.signal?.addEventListener("abort", () => {
            clearInterval(ping)
            if (sink) sinks.delete(sink)
            try {
              controller.close()
            } catch {}
          })
        },
      })
      return new Response(stream, {
        status: 200,
        headers: { "content-type": "text/event-stream", "cache-control": "no-cache" },
      })
    }

    // Live daemon link state for the footer's connection indicator. Reads the
    // Socket.IO `connected` flag straight from this closure — no cross-module
    // signal (which fails to share reactivity across the browser-conditions
    // build). The footer polls this.
    // One-shot SNAPSHOT for a freshly-mounted component (NOT polled) — the live
    // updates arrive via the pushed digitorn.connection event on the event stream.
    if (path === "/digitorn/connection" && method === "GET") {
      ensureSocket()
      return jsonRes(connProps())
    }

    // One-shot SNAPSHOT of context occupancy + spend (NOT polled). Live updates
    // arrive via the pushed digitorn.context event. Sourced from ctxBySession,
    // fed by the daemon's context_tokens / cost_update events.
    if (path === "/digitorn/context" && method === "GET") {
      const sid = new URL(req.url).searchParams.get("session") ?? ""
      return jsonRes(ctxProps(sid))
    }

    // ───── Digitorn apps : list + switch (digitorn is a multi-app platform) ─────
    if (path === "/digitorn/apps" && method === "GET") {
      try {
        return jsonRes((await loadApps()).map((a) => ({ ...a, current: a.id === currentApp })))
      } catch {
        return jsonRes([])
      }
    }
    if (path === "/digitorn/app" && method === "POST") {
      const body = (await req.json().catch(() => ({}))) as Record<string, any>
      const next = String(body?.app_id ?? body?.id ?? "")
      if (next && next !== currentApp) {
        currentApp = next
        turns.clear()
        todoLists.clear()
        joined.clear()
        pendingApprovals.clear()
        parallelChildren.clear()
        questionReqs.clear()
        // quiet = just re-point (home quick-pick : the next prompt creates a
        // session in this app). Otherwise re-bootstrap to reload its session list.
        if (!body?.quiet) emit(wrap({ type: "server.instance.disposed", properties: {} }))
      }
      return jsonRes({ app: currentApp })
    }

    // ───── ÉTAPE 1 : SESSIONS (real, from our daemon ; old Go CLI = reference) ─────
    const app = encodeURIComponent(currentApp)
    if (path === "/session" && method === "GET") {
      try {
        const r = await daemonFetch<{ sessions?: Record<string, any>[] }>(
          cfg,
          `/api/apps/${app}/sessions?limit=200`,
        )
        return jsonRes((r.sessions ?? []).map((s) => toOcSession(s, dir)))
      } catch {
        return jsonRes([]) // daemon down / auth : empty list, never crash the UI
      }
    }
    if (path === "/session" && method === "POST") {
      const body = (await req.json().catch(() => ({}))) as Record<string, any>
      const r = await daemonFetch<Record<string, any>>(cfg, `/api/apps/${app}/sessions`, {
        method: "POST",
        body: JSON.stringify({ title: body?.title, workdir: dir }),
      })
      return jsonRes(toOcSession(r, dir))
    }
    // ÉTAPE 3 : send a prompt → our daemon (+ join the room so the reply streams).
    const promptPost = path.match(/^\/session\/([^/]+)\/(prompt_async|prompt|message)$/)
    if (promptPost && method === "POST") {
      const sid = decodeURIComponent(promptPost[1])
      const body = (await req.json().catch(() => ({}))) as Record<string, any>
      // Only the user's typed text. opencode prepends SYNTHETIC parts (editor
      // context : "<system-reminder>…opened the file…") that aren't part of the
      // message — drop them so they don't pollute the daemon prompt.
      const text = Array.isArray(body?.parts)
        ? body.parts
            .filter((x: any) => x?.type === "text" && !x?.synthetic && x?.metadata?.kind !== "editor_context")
            .map((x: any) => x?.text ?? "")
            .join("\n")
        : String(body?.text ?? body?.content ?? "")
      dlog(`PROMPT route ${path} sid=${sid.slice(0, 8)} textLen=${text.length}`)
      joinSession(sid)
      try {
        await daemonFetch(cfg, `/api/apps/${app}/sessions/${encodeURIComponent(sid)}/messages`, {
          method: "POST",
          // opencode sends the picked agent name — which IS our mode (build/plan/…).
          body: JSON.stringify({ content: text, role: "user", mode: String(body?.agent ?? "") }),
        })
      } catch {}
      return jsonRes({})
    }
    // Escape → stop the AI mid-response. opencode's session.abort → our daemon's
    // per-session /abort (so the "press escape to stop" tip is actually true).
    const abortPost = path.match(/^\/session\/([^/]+)\/abort$/)
    if (abortPost && method === "POST") {
      const sid = decodeURIComponent(abortPost[1])
      try {
        await daemonFetch(cfg, `/api/apps/${app}/sessions/${encodeURIComponent(sid)}/abort`, { method: "POST" })
      } catch {}
      return jsonRes({})
    }
    // ÉTAPE 4b : permission reply → our daemon's approval registry. opencode's
    // {once,always}→approve, reject→deny. The route carries only requestID, so we
    // recover the session from pendingApprovals (set when we emitted the gate).
    const replyPost = path.match(/^\/permission\/([^/]+)\/reply$/)
    if (replyPost && method === "POST") {
      const reqID = decodeURIComponent(replyPost[1])
      const body = (await req.json().catch(() => ({}))) as Record<string, any>
      const action = String(body?.reply) === "reject" ? "denied" : "approved"
      const sidForReq = pendingApprovals.get(reqID) ?? ""
      try {
        await daemonFetch(cfg, `/api/apps/${app}/approve`, {
          method: "POST",
          body: JSON.stringify({ session_id: sidForReq, approval_id: reqID, action, reason: body?.message ?? "" }),
        })
      } catch {}
      return jsonRes({})
    }
    // ÉTAPE 4e : ask_user answer. opencode question.reply carries answers as
    // string[][] (per question → selected labels) ; our daemon takes the answer
    // as a plain string in `reason` on the SAME /approve endpoint (action=approve;
    // reject → deny). One question → flatten the selected labels.
    const qReply = path.match(/^\/question\/([^/]+)\/(reply|reject)$/)
    if (qReply && method === "POST") {
      const reqID = decodeURIComponent(qReply[1])
      const reject = qReply[2] === "reject"
      const body = (await req.json().catch(() => ({}))) as Record<string, any>
      const answer = Array.isArray(body?.answers)
        ? body.answers.map((a: any) => (Array.isArray(a) ? a.join(", ") : String(a ?? ""))).filter(Boolean).join("; ")
        : ""
      const sidForReq = pendingApprovals.get(reqID) ?? ""
      try {
        await daemonFetch(cfg, `/api/apps/${app}/approve`, {
          method: "POST",
          body: JSON.stringify({ session_id: sidForReq, approval_id: reqID, action: reject ? "denied" : "approved", reason: reject ? "" : answer }),
        })
      } catch {}
      return jsonRes({})
    }
    // ÉTAPE 2 : message history of a session → opencode {info,parts}[].
    const msgList = path.match(/^\/session\/([^/]+)\/message$/)
    if (msgList && method === "GET") {
      const sid = decodeURIComponent(msgList[1])
      joinSession(sid) // opening a session joins its room so live events stream
      try {
        const r = await daemonFetch<{ messages?: Record<string, any>[] }>(
          cfg,
          `/api/apps/${app}/sessions/${encodeURIComponent(sid)}/history?limit=2000`,
        )
        return jsonRes(toOcMessages(sid, r.messages ?? [], dir))
      } catch {
        return jsonRes([])
      }
    }
    // Per-session todo list (maintained live from todo_* events). diff stays [].
    const todoGet = path.match(/^\/session\/([^/]+)\/todo$/)
    if (todoGet && method === "GET") {
      return jsonRes(todosFor(decodeURIComponent(todoGet[1])))
    }
    if (method === "GET" && /^\/session\/[^/]+\/diff$/.test(path)) {
      return jsonRes([])
    }
    const sessOne = path.match(/^\/session\/([^/]+)$/)
    if (sessOne && sessOne[1] !== "status" && method === "GET") {
      try {
        const r = await daemonFetch<{ sessions?: Record<string, any>[] }>(
          cfg,
          `/api/apps/${app}/sessions?limit=500`,
        )
        const found = (r.sessions ?? []).find((s) => String(s.session_id ?? s.id) === sessOne[1])
        return jsonRes(toOcSession(found ?? { session_id: sessOne[1] }, dir))
      } catch {
        return jsonRes(toOcSession({ session_id: sessOne[1] }, dir))
      }
    }

    // Boot stubs — exact shapes the sync layer expects (see context/sync.tsx +
    // types.gen.ts). Filled with real daemon data feature by feature.
    if (method === "GET") {
      switch (path) {
        case "/config":
          return jsonRes({}) // Config (all fields optional)
        case "/config/providers": // populates data.provider + provider_default
          return jsonRes({ providers: [dgProvider], default: dgDefault })
        case "/path":
          return jsonRes({ home: dir, state: dir, config: dir, worktree: dir, directory: dir })
        case "/project":
        case "/project/current":
          return jsonRes({
            id: "digitorn",
            worktree: dir,
            name: currentApp,
            time: { created: now, updated: now },
            sandboxes: [],
          })
        case "/provider": // provider.list → provider_next shape {all,default,connected}
          return jsonRes({ all: [dgProvider], default: dgDefault, connected: ["digitorn"] })
        case "/provider/auth":
          return jsonRes({})
        case "/agent": {
          // app.agents → our digitorn MODES as opencode primary agents, so Tab
          // cycles them (build/plan/…) exactly like the old CLI's mode picker.
          const modes = await loadModes(currentApp)
          if (modes.length)
            return jsonRes(modes.map((m) => ({ name: m.id, description: m.description || m.label, mode: "primary", native: true, permission: {}, options: {} })))
          return jsonRes([{ name: "build", mode: "primary", native: true, permission: {}, options: {} }])
        }
        case "/session":
          return jsonRes([]) // ÉTAPE 1 : real session list
        case "/session/status":
          return jsonRes({})
        // Non-blocking boot stores : arrays vs objects matter for reconcile().
        case "/command":
        case "/lsp":
        case "/formatter":
          return jsonRes([])
        case "/mcp":
        case "/vcs":
          return jsonRes({})
      }
    }

    // Anything not yet wired : a benign empty 200 so the UI never hard-crashes
    // on an un-implemented call. Each becomes real, one feature at a time.
    return jsonRes({})
  }) as typeof fetch
}

// toOcMessages translates OUR flat history (Go CLI: {messages:[{seq,role,content,ts}]},
// the reference) into opencode's {info: Message, parts: Part[]}[] model. ÉTAPE 2 :
// user/assistant text only — tool calls become parts in a later step.
function toOcMessages(
  sessionID: string,
  msgs: Record<string, any>[],
  dir: string,
): Array<{ info: Record<string, unknown>; parts: Record<string, unknown>[] }> {
  const out: Array<{ info: Record<string, unknown>; parts: Record<string, unknown>[] }> = []
  let lastUserID = ""
  for (const m of msgs) {
    const role = m.role
    if (role !== "user" && role !== "assistant") continue // tool/system → later
    const content = String(m.content ?? "")
    // Assistant tool rounds carry a tool_calls[] array ({id,name,arguments,
    // status}; the daemon merges call+result, no output text persisted). Render
    // each as a completed ToolPart. Skip a round that has neither text nor tools.
    const toolCalls: any[] = role === "assistant" && Array.isArray(m.tool_calls) ? m.tool_calls : []
    if (role === "assistant" && !content.trim() && toolCalls.length === 0) continue
    const id = `${sessionID}:m:${padNum(m.seq ?? out.length)}`
    const created = toMs(m.ts) // our daemon sends ts as an ISO string
    let pord = 0
    const parts: Record<string, unknown>[] = []
    if (content) parts.push({ id: `${id}:p${padNum(pord++, 4)}`, sessionID, messageID: id, type: "text", text: content })
    for (const tc of toolCalls) {
      if (isHiddenTool(tc?.name)) continue // bookkeeping tools never show
      const { tool: name, input } = mapTool(tc?.name, tc?.arguments ?? {})
      const ok = String(tc?.status ?? "completed") === "completed"
      // run_parallel : expand into native children (no wrapper), same as live.
      const tasks = (tc?.arguments as any)?.tasks
      if (name === "run_parallel" && Array.isArray(tasks)) {
        const base = `${id}:p${padNum(pord++, 4)}`
        tasks.forEach((task: any, i: number) => {
          if (isHiddenTool(task?.tool)) return
          const m = mapTool(task?.tool, task?.args ?? {})
          parts.push({
            id: `${base}.${padNum(i, 3)}`,
            sessionID,
            messageID: id,
            type: "tool",
            callID: `${tc?.id ?? id}:${i}`,
            tool: m.tool,
            state: { status: "completed", input: m.input, output: "", title: m.tool, metadata: {}, time: { start: created, end: created } },
          })
        })
        continue
      }
      parts.push({
        id: `${id}:p${padNum(pord++, 4)}`,
        sessionID,
        messageID: id,
        type: "tool",
        callID: String(tc?.id ?? `${id}:${pord}`),
        tool: name,
        state: ok
          ? { status: "completed", input, output: "", title: name, metadata: {}, time: { start: created, end: created } }
          : { status: "error", input, error: String(tc?.status ?? "error"), time: { start: created, end: created } },
      })
    }
    if (role === "user") {
      lastUserID = id
      out.push({
        info: {
          id,
          sessionID,
          role: "user",
          time: { created },
          agent: "build",
          model: { providerID: "digitorn", modelID: "build" },
        },
        parts,
      })
    } else {
      out.push({
        info: {
          id,
          sessionID,
          role: "assistant",
          time: { created, completed: created },
          parentID: lastUserID,
          modelID: "build",
          providerID: "digitorn",
          mode: "build",
          agent: "build",
          path: { cwd: dir, root: dir },
          cost: 0,
          tokens: { input: 0, output: 0, reasoning: 0, cache: { read: 0, write: 0 } },
        },
        parts,
      })
    }
  }
  return out
}

// connectDaemonEvents opens the Socket.IO `/events` connection with our daemon's
// exact handshake ({token, user_id}), joins the session room, and hands every
// raw Envelope to onEnvelope. Returns a disposer. The Envelope→GlobalEvent
// translation is done by the caller (sdk.tsx), which holds the TUI's emitter.
export function connectDaemonEvents(
  cfg: DigitornConfig,
  sessionID: string | undefined,
  onEnvelope: (e: DaemonEnvelope) => void,
  onConnected?: () => void,
): { close: () => void; join: (sid: string) => void } {
  const sock: Socket = io(cfg.url + "/events", {
    transports: ["websocket"],
    auth: { token: cfg.token, user_id: cfg.userID },
  })

  let lastSeq = 0
  const join = (sid: string) => {
    if (!sid) return
    sock.emit("join_session", { session_id: sid })
    sock.emit("replay", { session_id: sid, since: lastSeq })
  }

  sock.on("connect", () => {
    onConnected?.()
    if (sessionID) join(sessionID)
  })
  sock.on("event", (env: DaemonEnvelope) => {
    if (typeof env?.seq === "number" && env.seq > lastSeq) lastSeq = env.seq
    onEnvelope(env)
  })

  return {
    close: () => sock.close(),
    join,
  }
}
