// Digitorn adapter — the bridge that lets opencode's STOCK TUI run on OUR Go
// daemon instead of opencode's backend. It lives entirely here, in the fork's
// client layer : NO separate proxy process, NO daemon changes. sdk.tsx swaps
// opencode's createOpencodeClient + SSE for this when DIGITORN_URL is set.
//
// This file is the protocol-grounded FOUNDATION : config/auth + the REST helper
// + the Socket.IO event bridge to our daemon. The opencode-client method surface
// and the Envelope→GlobalEvent translation are layered on top of it.
import { readFileSync, appendFileSync, writeFileSync, readdirSync } from "node:fs"
import { homedir } from "node:os"
import { join } from "node:path"
import { createHash } from "node:crypto"
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
// Build marker (logged ONCE when this module is freshly loaded) — proves whether
// the running process actually transpiled the latest adapter or is serving stale
// --watch / Bun-cache code.
dlog("=== DIGITORN ADAPTER BUILD MARKER: skills-cache-v3 ===")

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

// Set by digitornFetch once the adapter is live ; called by applyDigitornCredentials
// after /connect signs in, to swap the bearer on the running REST + socket layer.
let reloginHook: ((token: string, userID: string) => void) | undefined

// applyDigitornCredentials makes a fresh sign-in take effect WITHOUT a restart :
// the running adapter swaps to the new token (REST immediately, socket on its next
// connect). No-op if the adapter isn't up yet — digitornConfig() will read the
// freshly-written ~/.digitorn/credentials.json on the next boot anyway.
export function applyDigitornCredentials(creds: { access_token?: string; user_id?: string }): void {
  reloginHook?.(creds.access_token ?? "", creds.user_id ?? "")
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

// Slash commands we expose, each backed by an existing daemon session endpoint
// (dispatched in POST /session/{id}/command). `template`/`hints` are required by
// opencode's Command type; we run actions server-side, so template stays empty.
const DIGITORN_COMMANDS = [
  { name: "compact", description: "Compact the conversation to free up context", template: "", hints: [], source: "command" },
  { name: "fork", description: "Fork this session into a new one", template: "", hints: [], source: "command" },
  { name: "export", description: "Export this conversation as Markdown", template: "", hints: [], source: "command" },
]

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
// formFieldToQuestion maps ONE ask_user form field to an opencode question (a tab
// in the multi-question UI). select → single-select, multiselect → multi-select,
// boolean → Yes/No, everything else → free-text. A field description rides in the
// question text, and a default pre-fills (`value`). select/multiselect keep the
// custom-answer field unless allow_custom:false.
export const formFieldToQuestion = (f: any): Record<string, unknown> => {
  const name = String(f?.name ?? "")
  const label = String(f?.label ?? name) || name
  const type = String(f?.type ?? "").toLowerCase()
  const desc = String(f?.description ?? "")
  const opts: string[] = Array.isArray(f?.options) ? f.options.map(String) : []
  const def = f?.default
  const question = desc ? `${label} — ${desc}` : label
  const header = label.slice(0, 30)
  const allowCustom = f?.allow_custom !== false
  if (type === "multiselect") {
    return { question, header, options: opts.map((o) => ({ label: o, description: "" })), multiple: true, custom: allowCustom }
  }
  if (type === "select" || (opts.length > 0 && type !== "boolean")) {
    return { question, header, options: opts.map((o) => ({ label: o, description: "" })), multiple: false, custom: allowCustom, ...(def != null ? { value: String(def) } : {}) }
  }
  if (type === "boolean") {
    const v = def === true || String(def).toLowerCase() === "true" ? "Yes" : def != null ? "No" : undefined
    return { question, header, options: [{ label: "Yes", description: "" }, { label: "No", description: "" }], multiple: false, custom: false, ...(v ? { value: v } : {}) }
  }
  return { question, header, options: [], multiple: false, custom: true, ...(def != null ? { value: String(def) } : {}) }
}

// encodeQuestionReply turns opencode's per-question answers (string[][]) into the
// single `reason` string the daemon's /approve expects, per the question shape :
// form → JSON object keyed by field name (numbers/booleans coerced, multiselect →
// array) ; multi-select → comma-joined ; text/single/content → the value.
export const encodeQuestionReply = (
  shape: { mode: "plain" | "content" | "multi" | "form"; fields?: { name: string; type: string }[] } | undefined,
  answers: string[][],
): string => {
  if (shape?.mode === "form" && shape.fields) {
    const obj: Record<string, unknown> = {}
    shape.fields.forEach((f, i) => {
      const sel = answers[i] ?? []
      if (f.type === "multiselect") {
        obj[f.name] = sel
        return
      }
      const v = sel[0] ?? ""
      if (["number", "float", "range", "rating", "int", "integer"].includes(f.type)) {
        const n = Number(v)
        obj[f.name] = v !== "" && !Number.isNaN(n) ? (f.type === "int" || f.type === "integer" ? Math.trunc(n) : n) : v
      } else if (f.type === "boolean") {
        obj[f.name] = v === "Yes" || v.toLowerCase() === "true"
      } else {
        obj[f.name] = v
      }
    })
    return JSON.stringify(obj)
  }
  // multi-select AND single/text/content all collapse to the first question's
  // labels ; comma-join so the daemon's multi-select splitter sees each pick.
  return (answers[0] ?? []).join(", ")
}

const spawnInput = (args: unknown): Record<string, unknown> => {
  const a = (args ?? {}) as Record<string, any>
  const full = String(a.task ?? a.prompt ?? "")
  const seed = String(a.memory_seed ?? "").replace(/^\s*goal\s*:\s*/i, "").trim()
  const short = (seed || full.split("\n").map((s) => s.trim()).find(Boolean) || "Subagent").trim()
  return {
    description: short.length > 80 ? short.slice(0, 79) + "…" : short,
    // The daemon requires an agent/specialist name to spawn (see meta/agent.go),
    // so this is normally "explore"/"general"/… . `||` (not `??`) so an empty
    // string also falls through → "" marks a nameless/anonymous agent, which the
    // renderer labels "Subagent" rather than mislabelling it the "general" specialist.
    subagent_type: String(a.agent || a.kind || ""),
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

// The daemon appends an interruption marker to a partial assistant message so the
// LLM has resume context (engine.go persistInterruptedAssistant). That marker is
// LLM-facing, NOT for the user — strip it from anything we DISPLAY (the daemon keeps
// it in the persisted content for the model). Matched on its stable prefix so a
// wording tweak doesn't desync. hadInterruptMarker → flag the message interrupted.
const INTERRUPT_MARKER = /\n*\[Response interrupted before completion[^\]]*\]\s*$/
const stripInterruptMarker = (s: string): string => s.replace(INTERRUPT_MARKER, "")
const hadInterruptMarker = (s: string): boolean => INTERRUPT_MARKER.test(s)

// manifestModel pulls the LLM model the app's entry agent is configured to use
// from the compiled manifest (GET /api/apps/{id}/manifest) — ported from the old
// CLI's manifestModel. The manifest serialises the daemon's AppDefinition (yaml
// tags), so JSON keys can be lower OR Capitalised; navigate defensively. Falls
// back to a top-level brain. Empty when nothing declares a model.
const manifestModel = (raw: any): string => {
  const pick = (o: any): string => {
    const brain = o?.brain ?? o?.Brain
    const m = brain?.model ?? brain?.Model
    return typeof m === "string" ? m : ""
  }
  const agents = raw?.agents ?? raw?.Agents
  if (Array.isArray(agents)) {
    for (const a of agents) {
      const m = pick(a)
      if (m) return m
    }
  }
  return pick(raw)
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

// --- @ file completions ----------------------------------------------------
// opencode's composer calls GET /find/file?query= for the @-mention picker and
// expects a string[] of workdir-relative paths. We answer it locally : the TUI
// process has fs access and the session workdir IS process.cwd() (the adapter
// binds new sessions to it), so a cached recursive walk + fuzzy filename match
// gives instant completions with no daemon round-trip.
const FIND_IGNORE = new Set([
  "node_modules", ".git", "dist", "build", "out", ".next", ".nuxt", "vendor",
  "target", ".venv", "venv", "__pycache__", ".cache", "coverage", ".idea",
  ".turbo", ".parcel-cache", ".svelte-kit", "bin", "obj",
])
let findCache: { root: string; files: string[]; at: number } | null = null

function walkWorkdir(root: string, cap = 20000): string[] {
  const out: string[] = []
  const stack: string[] = [""]
  while (stack.length && out.length < cap) {
    const rel = stack.pop()!
    let entries
    try {
      entries = readdirSync(rel ? join(root, rel) : root, { withFileTypes: true })
    } catch {
      continue
    }
    for (const e of entries) {
      if (out.length >= cap) break
      const name = String(e.name)
      const childRel = rel ? `${rel}/${name}` : name
      if (e.isDirectory()) {
        // Skip the ignore set + hidden dirs (.git, .digitorn shadow, .vscode…) —
        // matches ripgrep's default, which is what opencode's native find does.
        if (!FIND_IGNORE.has(name) && !name.startsWith(".")) stack.push(childRel)
      } else if (e.isFile()) {
        out.push(childRel)
      }
    }
  }
  return out
}

// 5s-cached recursive file list of the workdir (re-walking on every keystroke
// would be wasteful ; the composer queries on each character).
function workdirFiles(root: string): string[] {
  const now = Date.now()
  if (findCache && findCache.root === root && now - findCache.at < 5000) return findCache.files
  const files = walkWorkdir(root)
  findCache = { root, files, at: now }
  return files
}

const isSubseq = (q: string, s: string): boolean => {
  let i = 0
  for (let j = 0; j < s.length && i < q.length; j++) if (s[j] === q[i]) i++
  return i === q.length
}

// Higher = better ; null = no match. Basename hits beat path hits beat
// subsequence ; shorter paths break ties (so "app.tsx" ranks above a deep file).
function fuzzyFileScore(p: string, q: string): number | null {
  const base = p.slice(p.lastIndexOf("/") + 1)
  let idx = base.indexOf(q)
  if (idx >= 0) return 1000 - idx - base.length * 0.1
  idx = p.indexOf(q)
  if (idx >= 0) return 600 - idx - p.length * 0.1
  if (isSubseq(q, base)) return 300 - base.length * 0.1
  if (isSubseq(q, p)) return 150 - p.length * 0.1
  return null
}

function findFiles(root: string, query: string, limit: number): string[] {
  const files = workdirFiles(root)
  const q = query.trim().toLowerCase()
  if (!q) return files.slice(0, limit)
  const scored: Array<[number, string]> = []
  for (const f of files) {
    const s = fuzzyFileScore(f.toLowerCase(), q)
    if (s !== null) scored.push([s, f])
  }
  scored.sort((a, b) => b[0] - a[0] || a[1].length - b[1].length)
  const out: string[] = []
  for (let i = 0; i < scored.length && out.length < limit; i++) out.push(scored[i][1])
  return out
}

// digitornFetch returns a fetch implementation routing opencode's API surface.
export function digitornFetch(cfg: DigitornConfig): typeof fetch {
  // The agent's working directory = the launch dir, sent as `workdir` on session
  // create (the daemon honors an absolute client workdir over its sandbox, like
  // the Go CLI passing os.Getwd()). DIGITORN_CWD decouples it from process.cwd()
  // for the dev launcher, which must `cd` into the package so Bun loads opencode's
  // bunfig/tsconfig (JSX) — DIGITORN_CWD then carries the real project dir.
  const dir = process.env.DIGITORN_CWD?.trim() || process.cwd()
  const now = Date.now()
  // The digitorn LLM gateway (OpenAI-compatible). The /digitorn/models route below
  // reads its catalog from here. Local default; overridable for non-local setups.
  const gatewayURL = (process.env.DIGITORN_GATEWAY_URL ?? "http://127.0.0.1:8002/v1").replace(/\/+$/, "")

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
  let appsCache: Array<{ id: string; name: string; description: string; category: string; icon: string; color: string; version: string }> | undefined
  const loadApps = async () => {
    if (appsCache) return appsCache
    const r = await daemonFetch<any>(cfg, `/api/apps`)
    const list: Record<string, any>[] = (Array.isArray(r) ? r : (r?.apps ?? [])).filter((a: any) => a.enabled !== false)
    appsCache = await Promise.all(
      list.map(async (a) => {
        const id = String(a.app_id ?? a.id ?? "")
        let icon = a.icon
        let color = a.color
        let version = a.version
        if (icon == null || color == null || version == null) {
          try {
            const d = await daemonFetch<any>(cfg, `/api/apps/${encodeURIComponent(id)}`)
            icon = icon ?? d?.icon
            color = color ?? d?.color
            version = version ?? d?.version
          } catch {}
        }
        return { id, name: String(a.name ?? id), description: String(a.description ?? ""), category: String(a.category ?? ""), icon: String(icon ?? ""), color: String(color ?? ""), version: String(version ?? "") }
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
  // The REAL model is the app's (chosen server-side) — read it from the manifest
  // like the old CLI did, so the prompt footer shows it instead of the "Digitorn"
  // placeholder. Cached per app (manifests don't change at runtime).
  const modelCache = new Map<string, string>()
  const loadModel = async (appId: string): Promise<string> => {
    const hit = modelCache.get(appId)
    if (hit !== undefined) return hit
    let model = ""
    try {
      const m = await daemonFetch<any>(cfg, `/api/apps/${encodeURIComponent(appId)}/manifest`, {
        signal: AbortSignal.timeout(3000), // never let a slow manifest hang the caller
      })
      model = manifestModel(m)
    } catch {}
    modelCache.set(appId, model)
    return model
  }
  // Replace the prompt footer's "Digitorn Digitorn" (model.name + provider.name)
  // with the app's REAL model : set the model NAME to the manifest model and
  // BLANK the provider name so the line reads "Mode · <model>" (no duplicate).
  // Re-applies when currentApp changes (so an app switch + provider refetch
  // updates it). Falls back to the app id when no model is declared.
  let modelApplied: string | undefined
  const applyAppModel = async () => {
    if (modelApplied === currentApp) return
    dgModel.name = (await loadModel(currentApp)) || currentApp
    dgProvider.name = ""
    modelApplied = currentApp
  }

  // Gateway catalog grouped by kind. The hard 6s deadline is load-bearing: it is
  // what stops a slow/unreachable gateway from freezing the models dialog.
  type CatModel = { id: string; context: number; cat: string }
  const loadGatewayCatalog = async (): Promise<{ groups: { category: string; models: CatModel[] }[]; error?: string }> => {
    const build = async () => {
      const res = await fetch(`${gatewayURL}/models`, {
        headers: { authorization: `Bearer ${cfg.token}` },
        signal: AbortSignal.timeout(3000),
      })
      if (!res.ok) return { groups: [], error: `gateway returned HTTP ${res.status}` }
      const list: any[] = (await res.json())?.data ?? []
      const byKind = new Map<string, CatModel[]>()
      for (const m of list) {
        const id = String(m?.id ?? "")
        if (!id) continue
        const kind = String(m?.kind ?? "").trim() || "text"
        const context = Number(m?.max_context_tokens ?? 0)
        const cats = Array.isArray(m?.categories) ? m.categories.map(String).filter(Boolean) : []
        if (!byKind.has(kind)) byKind.set(kind, [])
        byKind.get(kind)!.push({ id, context, cat: cats.join(" · ") })
      }
      const KIND_RANK: Record<string, number> = { chat: 0, text: 0, image: 1, audio: 2, video: 3, embedding: 4 }
      const rank = (a: string, b: string) => (KIND_RANK[a] ?? 50) - (KIND_RANK[b] ?? 50) || a.localeCompare(b)
      const groups = [...byKind.keys()].sort(rank).map((category) => ({ category, models: byKind.get(category)! }))
      return { groups }
    }
    try {
      return await Promise.race([
        build(),
        new Promise<{ groups: never[]; error: string }>((r) =>
          setTimeout(() => r({ groups: [], error: "timed out loading models (gateway slow/unreachable)" }), 6000),
        ),
      ])
    } catch (e: any) {
      return { groups: [], error: `gateway unreachable: ${String(e?.message ?? e)}` }
    }
  }

  // ── ÉTAPE 3 : live chat plumbing ──────────────────────────────────────────
  // ONE Socket.IO to our daemon, fanned out to every open /event SSE stream,
  // translating each Envelope into opencode GlobalEvents. Per-session turn state
  // builds opencode "parts" from our streaming deltas.
  const sinks = new Set<(ev: unknown) => void>()
  const joined = new Set<string>()
  // The most-recently-joined (active) session + the highest seq seen per session.
  // On socket reconnect we re-join lastJoined and replay since its lastSeq, so a
  // turn that was in-flight during a daemon drop catches up instead of hanging.
  let lastJoined = ""
  const lastSeqOf = new Map<string, number>()
  let lastEventAt = 0 // wall-clock of the last envelope received (any, incl. replayed)
  // The dead-turn watchdog must distinguish HISTORICAL replayed events (which the
  // daemon re-emits on reconnect to fill the gap) from genuine LIVE progress. A
  // daemon that CRASHED mid-turn replays only historical events then goes silent —
  // those must NOT be mistaken for "the turn is alive". So we flip `replaying`
  // true while a replay is in flight (until the daemon's `replay_done`), and only
  // bump `lastLiveEventAt` for events that arrive OUTSIDE that window. The watchdog
  // keys on lastLiveEventAt, so replayed events can't keep a dead turn spinning.
  let replaying = false
  let lastLiveEventAt = 0
  // Sub-agent child sessions we've already announced (session.updated) — once per
  // child, so opencode's sub-session UI sees them without duplicate inserts.
  const registeredChildren = new Set<string>()
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
  // Per-turn WORK state, mirroring the Go CLI's renderShimmer counters : the live
  // OUTPUT-token count (banked across rounds so a multi-round/tool turn never
  // resets mid-turn), an `exact` flag (true once the provider's token_usage lands,
  // dropping the "~" estimate marker), a char fallback (chars/4 before the first
  // live count), and a `compacting` flag (between context_compacting/compacted).
  // Pushed as digitorn.work so the in-chat working line shows "◆ ~Xk tokens" /
  // "◆ ⟢ compacting context…" while a turn runs. Reset at turn_started.
  type Work = { tokens: number; base: number; rounds: number; exact: boolean; chars: number; compacting: boolean }
  const workBySession = new Map<string, Work>()
  const workFor = (sid: string): Work => {
    let v = workBySession.get(sid)
    if (!v) {
      v = { tokens: 0, base: 0, rounds: 0, exact: false, chars: 0, compacting: false }
      workBySession.set(sid, v)
    }
    return v
  }
  const workProps = (sid: string) => {
    const w = workFor(sid)
    const tokens = w.tokens > 0 ? w.tokens : w.chars > 0 ? Math.floor(w.chars / 4) : 0
    return { session: sid, tokens, exact: w.exact, compacting: w.compacting }
  }
  const broadcastWork = (sid: string) => emit(wrap({ type: "digitorn.work", properties: workProps(sid) }))
  // Once we've connected at least once, a later connect_error means the daemon
  // dropped (stay red), vs the initial pre-connect attempts (yellow connecting).
  let everConnected = false
  // Pending tool-call gates : requestID → sessionID, so the permission reply
  // route (which only carries requestID) can address our daemon's /approve.
  const pendingApprovals = new Map<string, string>()
  // requestIDs that are ask_user questions (kind=="question"), so we resolve them
  // as opencode question.* events instead of permission.* on granted/denied.
  const questionReqs = new Set<string>()
  // Per question requestID : how to re-encode the user's answer for the daemon —
  // "form" rebuilds a JSON object (one field per asked sub-question), "multi"
  // comma-joins (the daemon splits on comma), "plain"/"content" pass the text.
  const questionShapes = new Map<string, { mode: "plain" | "content" | "multi" | "form"; fields?: { name: string; type: string }[] }>()
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
  type ToolRec = { partID: string; name: string; input: Record<string, unknown>; start: number; sessionId?: string; done?: boolean }
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
  // The ACTIVE digitorn app, pushed to the home active-app indicator whenever it
  // changes (the picker POSTs /digitorn/app). Same push pattern as conn/context.
  const broadcastApp = async () => {
    try {
      const cur = (await loadApps()).find((a) => a.id === currentApp)
      emit(wrap({ type: "digitorn.app", properties: { id: currentApp, name: cur?.name ?? currentApp, icon: cur?.icon ?? "", color: cur?.color ?? "" } }))
    } catch {}
  }
  // Drives opencode's prompt status row : type:"busy" while a turn runs → the
  // spinner + "esc interrupt" show; type:"idle" hides them. We never emitted this
  // (only session.idle, a different event), so the spinner stayed hidden.
  // Sessions currently in a daemon-side retry backoff (turn_retry). Tracked so the
  // first content event after a successful retry flips the prompt row back from
  // "retrying…" to busy ; cleared whenever the session goes idle.
  const retrying = new Set<string>()
  // Client-side prompt queue (opencode parity — but WE sequence it, the daemon only
  // ever sees ONE prompt at a time). `busy` mirrors the turn state we drive; a
  // prompt typed while busy is held in `promptQueue` + rendered as a synthetic
  // QUEUED message, and flushed the instant the session goes idle.
  const busy = new Set<string>()
  const promptQueue = new Map<string, { content: string; mode: string; skill: string; msgID: string }[]>()
  const emitStatus = (sid: string, type: "busy" | "idle") => {
    if (type === "idle") retrying.delete(sid)
    emit(wrap({ type: "session.status", properties: { sessionID: sid, status: { type } } }))
    if (type === "busy") busy.add(sid)
    else if (busy.delete(sid)) flushQueue(sid) // real busy→idle transition → start the next queued prompt
  }
  // Send the next queued prompt (if any) to the daemon as a fresh turn. The
  // synthetic QUEUED message is removed first so the daemon's real user_message
  // takes its correct chronological place. emitStatus("busy") runs SYNCHRONOUSLY
  // (busy.add) so a redundant idle event can never double-send the next item.
  const flushQueue = (sid: string) => {
    const q = promptQueue.get(sid)
    if (!q || q.length === 0) return
    const next = q.shift()!
    emit(wrap({ type: "message.removed", properties: { sessionID: sid, messageID: next.msgID } }))
    emitStatus(sid, "busy")
    void daemonFetch(cfg, `/api/apps/${encodeURIComponent(currentApp)}/sessions/${encodeURIComponent(sid)}/messages`, {
      method: "POST",
      body: JSON.stringify({ content: next.content, role: "user", mode: next.mode, ...(next.skill ? { skill: next.skill } : {}) }),
    }).catch((e: any) => translate({ type: "error", session_id: sid, payload: { error: String(e?.message ?? e), source: "send" } }))
  }
  // Settle a turn that was killed without the usual tool_result/turn_ended : an
  // abort (session_interrupted) OR a turn left dead by a daemon restart (detected
  // after reconnect). Every still-running tool/task part → "Interrupted" (stops
  // the spinners, incl. the sub-agent Tasks), the message → MessageAbortedError,
  // the session → idle. Idempotent : a turn already gone just emits idle.
  const interruptTurn = (sid: string) => {
    const cur = turns.get(sid)
    if (cur) {
      finalizeReasoning(sid, cur)
      const now = Date.now()
      const running = new Map<string, { callID: string; name: string; input: Record<string, unknown>; sessionId?: string }>()
      for (const [callID, rec] of cur.tools)
        if (!rec.done) running.set(rec.partID, { callID, name: rec.name, input: rec.input, sessionId: rec.sessionId })
      for (const kids of parallelChildren.values())
        for (const kid of kids)
          if (!running.has(kid.partID)) running.set(kid.partID, { callID: kid.callID, name: kid.name, input: kid.input })
      for (const [partID, r] of running)
        partUpdated(sid, {
          id: partID,
          sessionID: sid,
          messageID: cur.msgID,
          type: "tool",
          callID: r.callID,
          tool: r.name,
          state: { status: "error", input: r.input, error: "Interrupted", ...(r.sessionId ? { metadata: { sessionId: r.sessionId } } : {}), time: { start: now, end: now } },
        })
      cur.error = { name: "MessageAbortedError", data: { message: "Interrupted" } }
      emit(wrap({ type: "message.updated", properties: { sessionID: sid, info: asstInfo(sid, cur.msgID, cur.created, now, cur.error) } }))
    }
    turns.delete(sid)
    parallelChildren.clear()
    workBySession.delete(sid)
    broadcastWork(sid)
    emitStatus(sid, "idle")
    emit(wrap({ type: "session.idle", properties: { sessionID: sid } }))
  }
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
  // Last assistant message per session, kept even AFTER the turn is deleted — so a
  // trailing `error` event (the daemon sends turn_ended BEFORE the error) can attach
  // to the turn that actually failed instead of spawning a detached, empty error
  // bubble below it.
  const lastAsst = new Map<string, { msgID: string; created: number }>()
  const ensureTurn = (sid: string, seq: number): Turn => {
    let cur = turns.get(sid)
    if (!cur) {
      const created = Date.now()
      cur = { msgID: `${sid}:m:${padNum(seq)}`, created, buf: new Map(), tools: new Map() }
      turns.set(sid, cur)
      lastAsst.set(sid, { msgID: cur.msgID, created })
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

  // Workspace changes → opencode's SnapshotFileDiff[] / VcsFileDiff[] (identical
  // fields). The daemon's shadow git tracks every pending edit vs the session
  // baseline; GET /workspace/changes?include_diffs=1 returns the unified patch +
  // line stats per file. opencode's native <diff> element parses `patch` itself,
  // so we hand it straight through. ONE session-scoped source feeds both the
  // sidebar files panel (session.diff event) and the /diff viewer (vcs.diff /
  // session.diff REST). Approved files come back with empty pending diff/stats →
  // filtered out so only the live change set shows.
  const fetchSessionDiff = async (sid: string): Promise<Record<string, unknown>[]> => {
    if (!sid) return []
    const r = await daemonFetch<{ files?: Record<string, any>[] }>(
      cfg,
      `/api/apps/${encodeURIComponent(currentApp)}/sessions/${encodeURIComponent(sid)}/workspace/changes?include_diffs=1`,
    )
    return (r.files ?? [])
      .map((f) => ({
        file: String(f.path ?? ""),
        patch: String(f.unified_diff_pending ?? ""),
        additions: Number(f.insertions_pending ?? 0),
        deletions: Number(f.deletions_pending ?? 0),
        status: f.status === "added" || f.status === "deleted" ? f.status : "modified",
      }))
      .filter((f) => f.file && (f.patch || f.additions || f.deletions))
  }
  const emitSessionDiff = (sid: string) =>
    void fetchSessionDiff(sid)
      .then((diff) => emit(wrap({ type: "session.diff", properties: { sessionID: sid, diff } })))
      .catch(() => {})

  const translate = (env: DaemonEnvelope) => {
    const sid = env.session_id ?? ""
    if (!sid) return
    const p: any = env.payload ?? {}
    const seq = typeof env.seq === "number" ? env.seq : Date.now()
    const t = Date.now()
    switch (env.type) {
      case "turn_started": {
        emitStatus(sid, "busy") // turn running → opencode shows the spinner + "esc interrupt"
        workBySession.set(sid, { tokens: 0, base: 0, rounds: 0, exact: false, chars: 0, compacting: false })
        broadcastWork(sid)
        // Materialize the assistant message NOW so the working indicator (the footer
        // diamond) shows from the turn's start — even if the turn errors before
        // producing any content. Subsequent assistant events reuse this same turn.
        ensureTurn(sid, seq)
        break
      }
      case "turn_retry": {
        // Daemon is auto-retrying a transient provider/network fault. Drive
        // opencode's native retry status (prompt row : "<error> [retrying in Ns
        // attempt #N]"). next = now + backoff for the live countdown. This event is
        // DURABLE, so a client reconnecting mid-retry replays it and shows it too.
        const rp: any = p ?? {}
        retrying.add(sid)
        emit(
          wrap({
            type: "session.status",
            properties: {
              sessionID: sid,
              status: {
                type: "retry",
                message: String(rp.message ?? "Retrying…"),
                attempt: Number(rp.attempt ?? 2),
                next: Date.now() + (Number(rp.retry_in_ms ?? 0) || 0),
              },
            },
          }),
        )
        break
      }
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
        if (retrying.delete(sid)) emitStatus(sid, "busy") // generation resumed after a retry
        const cur = ensureTurn(sid, seq)
        finalizeReasoning(sid, cur) // thinking → answer : close the thinking block
        cur.streamed = true
        if (!cur.textPartID) cur.textPartID = nextPartID(cur, seq)
        const id = cur.textPartID
        const chunk = partsText(p)
        const text = (cur.buf.get(id) ?? "") + chunk
        cur.buf.set(id, text)
        partUpdated(sid, { id, sessionID: sid, messageID: cur.msgID, type: "text", text })
        // Live token counter for the working line. live_output_tokens is per-MESSAGE
        // (resets each round) → add it to the rounds already banked so a multi-round
        // turn never drops. chars is the fallback (chars/4) before the first count.
        const w = workFor(sid)
        w.chars += [...chunk].length
        const live = Number(env.live_output_tokens ?? (p as any).live_output_tokens ?? 0) || 0
        if (live > 0) w.tokens = w.base + live
        broadcastWork(sid)
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
        if (retrying.delete(sid)) emitStatus(sid, "busy") // generation resumed after a retry
        const callID = String(p.call_id ?? "")
        if (!callID || isHiddenTool(p.name)) break // bookkeeping tools never show
        const cur = ensureTurn(sid, seq)
        finalizeReasoning(sid, cur) // thinking → tool : close the thinking block
        cur.textPartID = undefined // any post-tool text opens a NEW part, after the tool
        const hasArgs = p.arguments && typeof p.arguments === "object"
        // Count the tokens the model spends EMITTING the tool call + its arguments,
        // not just final-answer text. live_output_tokens is per-message cumulative,
        // so add it onto the rounds already banked (Go CLI does this in its tool_call
        // streaming handler) — the working counter never stalls during tool rounds.
        {
          const live = Number(env.live_output_tokens ?? (p as any).live_output_tokens ?? 0) || 0
          if (live > 0) {
            const w = workFor(sid)
            w.tokens = w.base + live
            broadcastWork(sid)
          }
        }

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
              if (rec) rec.done = true // settled → a later interrupt won't re-settle it
              // A sub-agent emits NO turn_ended/session_idle for its own session, so
              // its assistant message never got time.completed → opencode's Task
              // duration (child assistant.completed − child user.created) was 0ms.
              // run_parallel resolving IS the "this sub-agent finished" signal, so
              // stamp the child assistant's completed here → the real duration shows.
              if (sessionId) {
                const ct = turns.get(sessionId)
                if (ct)
                  emit(wrap({ type: "message.updated", properties: { sessionID: sessionId, info: asstInfo(sessionId, ct.msgID, ct.created, t) } }))
              }
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
        rec.done = true // settled → a later interrupt won't re-settle it
        // A direct sub-agent (wait:false) emits NO completion event — its task part
        // settled at the spawn ACK, so its Task duration would stay 0ms until the
        // root turn ends. Bump the CHILD assistant's completed to each tool's time
        // so opencode's Task duration (completed − child user.created) grows LIVE
        // and ends at the sub-agent's last activity, not the whole turn's end.
        if (sid.includes("::agent::"))
          emit(wrap({ type: "message.updated", properties: { sessionID: sid, info: asstInfo(sid, cur.msgID, cur.created, t) } }))
        break
      }
      case "assistant_message": {
        if (retrying.delete(sid)) emitStatus(sid, "busy") // generation resumed after a retry
        const cur = ensureTurn(sid, seq)
        finalizeReasoning(sid, cur)
        // assistant_delta already streamed this step's text into its part —
        // re-emitting here would DUPLICATE it (a fresh part after a tool_call
        // cleared textPartID). Only materialize when nothing streamed (a
        // non-streaming provider that sends the message whole). Strip the LLM-only
        // interruption marker so the user never sees it (the daemon keeps it).
        const text = stripInterruptMarker(partsText(p))
        if (text && !cur.streamed) {
          const id = cur.textPartID ?? nextPartID(cur, seq)
          cur.textPartID = id
          cur.buf.set(id, text)
          partUpdated(sid, { id, sessionID: sid, messageID: cur.msgID, type: "text", text })
        }
        // Bank this round's output tokens so the next round's per-message
        // live_output_tokens adds ON TOP (keeps the turn counter monotonic across
        // tool-call rounds), exactly like the Go CLI's turnTokensBase/turnRounds.
        const w = workFor(sid)
        w.rounds += 1
        w.base = w.tokens
        // The turn stays alive : late tool_result / post-tool text still arrive.
        break
      }
      case "approval_request": {
        const id = String(p.id ?? "")
        if (!id) break
        pendingApprovals.set(id, sid)
        const cur = turns.get(sid)
        if (p.kind === "question") {
          // ask_user → opencode question.asked. Question text in `reason` ; the
          // interaction shape (choices/form/content/custom) lives in `payload`.
          questionReqs.add(id)
          const pl: any = p.payload ?? {}
          const q = String(p.reason ?? "Question")
          let questions: any[]
          let shape: { mode: "plain" | "content" | "multi" | "form"; fields?: { name: string; type: string }[] }
          if (Array.isArray(pl.form) && pl.form.length) {
            // One opencode question (tab) per form field ; the reply rebuilds the JSON.
            questions = pl.form.map((f: any) => formFieldToQuestion(f))
            shape = { mode: "form", fields: pl.form.map((f: any) => ({ name: String(f?.name ?? ""), type: String(f?.type ?? "").toLowerCase() })) }
          } else if (typeof pl.content === "string" && pl.content) {
            // Editable review : seed the free-text box with the content to edit in place.
            questions = [{ question: q, header: q.slice(0, 30), options: [], multiple: false, custom: true, value: pl.content }]
            shape = { mode: "content" }
          } else {
            const choices: string[] = Array.isArray(pl.choices) ? pl.choices.map(String) : []
            const multiple = Boolean(pl.allow_multiple)
            questions = [
              {
                question: q,
                header: q.slice(0, 30),
                options: choices.map((c) => ({ label: c, description: "" })),
                multiple,
                // The custom-answer field is ALWAYS offered on proposals unless the
                // agent set allow_custom:false — the user can answer the unforeseen.
                custom: choices.length === 0 ? true : pl.allow_custom !== false,
                ...(typeof pl.default === "string" && pl.default ? { value: pl.default } : {}),
              },
            ]
            shape = { mode: multiple ? "multi" : "plain" }
          }
          questionShapes.set(id, shape)
          emit(
            wrap({
              type: "question.asked",
              properties: { id, sessionID: sid, questions, tool: { messageID: cur?.msgID ?? "", callID: String(p.call_id ?? "") } },
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
        questionShapes.delete(id)
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
      case "workspace_changes": {
        // Transient live signal : the agent's pending edits changed (debounced
        // shadow-git status). Refetch the full diff (patch + stats) and push
        // opencode's session.diff so the sidebar files panel + any open /diff
        // viewer update without a manual reload. No durable seq is consumed.
        emitSessionDiff(sid)
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
        // Register the sub-agent's session as a CHILD of the parent (parentID set)
        // so opencode's NATIVE sub-session UI lights up : "ctrl+x ↓ view subagents"
        // drills into its full transcript and child cycling (←/→) works. The picker
        // filters parentID!==undefined, so children never pollute the session list.
        // Its messages already stream in via the ancestor fan-out → the drilled-in
        // view renders the sub-agent's real activity.
        if (!registeredChildren.has(childSid)) {
          registeredChildren.add(childSid)
          const inp = (found.rec.input ?? {}) as any
          const title = String(inp.description || inp.subagent_type || "Subagent")
          emit(
            wrap({
              type: "session.updated",
              properties: {
                info: {
                  id: childSid,
                  slug: childSid,
                  projectID: "digitorn",
                  directory: dir,
                  parentID: found.sid,
                  title,
                  version: "0.0.0",
                  time: { created: Date.now(), updated: Date.now() },
                },
              },
            }),
          )
        }
        break
      }
      // Turn failure → opencode's in-chat error banner. Our daemon emits a
      // type:"error" event (ErrorPayload : error/message/code/category/detail/
      // retry) on the failure path; we hang it on the assistant message's
      // info.error, which opencode renders as a red-bordered box. ensureTurn so
      // a failure with no streamed content still has a message to carry it.
      case "error": {
        const info = errorInfo(p)
        const cur = turns.get(sid)
        if (cur) {
          // Turn still in-flight → hang the error on its assistant message.
          finalizeReasoning(sid, cur)
          cur.error = info
          emit(wrap({ type: "message.updated", properties: { sessionID: sid, info: asstInfo(sid, cur.msgID, cur.created, t, info) } }))
        } else {
          // turn_ended already fired (daemon emits it BEFORE the error) → attach to
          // the turn that just failed (its assistant message), so the error renders
          // INSIDE that turn in the chat flow rather than as a detached empty bubble
          // pinned at the bottom. Only if there was never any assistant, mint one.
          const last = lastAsst.get(sid)
          if (last) {
            emit(wrap({ type: "message.updated", properties: { sessionID: sid, info: asstInfo(sid, last.msgID, last.created, t, info) } }))
          } else {
            const c = ensureTurn(sid, seq)
            c.error = info
            emit(wrap({ type: "message.updated", properties: { sessionID: sid, info: asstInfo(sid, c.msgID, c.created, t, info) } }))
          }
        }
        // An error ALWAYS ends the work : stop the spinner (the daemon's turn_ended
        // may precede the error, but a send-failure / mid-stream error might not get
        // one — never leave it spinning).
        workBySession.delete(sid)
        broadcastWork(sid)
        emitStatus(sid, "idle")
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
      case "token_usage":
      case "cost_update": {
        // The provider's EXACT output usage landed. tokens_out is only the LAST
        // round's completion, so snap to it (dropping the "~") only for a single-
        // round turn; for a multi-round turn keep the cumulative live estimate
        // (still ~). Never let the shown count drop. (Go CLI: token_usage handler.)
        const out = Number((p as any).tokens_out ?? 0) || 0
        if (out > 0) {
          const w = workFor(sid)
          if (w.rounds <= 1) {
            w.tokens = out
            w.exact = true
          } else if (out > w.tokens) {
            w.tokens = out
          }
          broadcastWork(sid)
        }
        break
      }
      case "context_compacting": {
        workFor(sid).compacting = true // working line shows "◆ ⟢ compacting context…"
        broadcastWork(sid)
        break
      }
      case "context_compacted": {
        workFor(sid).compacting = false
        broadcastWork(sid)
        break
      }
      // Escape / abort → the daemon stopped the turn AND its whole sub-agent tree
      // (abortSession: sessionRunner.Abort + agents.CancelAll + background cancel)
      // and emits this. The daemon sends NO tool_result/turn_ended for the killed
      // work, so we settle it ourselves : every still-running tool/task part →
      // "Interrupted" (stops the spinners, incl. the sub-agent Tasks), the message
      // → MessageAbortedError, the session → idle. Without this the daemon stops
      // but the UI keeps spinning — exactly the "abort doesn't stop everything" bug.
      case "session_interrupted": {
        interruptTurn(sid)
        break
      }
      case "turn_ended":
      case "session_idle": {
        // When the ROOT turn ends, every sub-agent is finished — but the daemon
        // never emits a child turn_ended/session_idle, so each child's assistant
        // message still lacks time.completed and opencode's Task duration reads
        // 0ms. Finalize every open child turn here (stamp completed) so the real
        // duration resolves, for BOTH run_parallel and direct spawns.
        if (!sid.includes("::agent::")) {
          for (const [csid, ct] of turns) {
            if (csid === sid || !csid.includes("::agent::")) continue
            emit(wrap({ type: "message.updated", properties: { sessionID: csid, info: asstInfo(csid, ct.msgID, ct.created, t) } }))
            turns.delete(csid)
          }
        }
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
        workBySession.delete(sid) // turn over → clear the working-line counters
        broadcastWork(sid)
        emitStatus(sid, "idle") // hide the prompt spinner / "esc interrupt"
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
      const reconnect = everConnected
      everConnected = true
      connState = "connected"
      broadcastConn()
      dlog(`socket CONNECTED id=${socket?.id} reconnect=${reconnect}`)
      // RECONNECT recovery : the daemon dropped our room memberships when the
      // socket died, and any events emitted while we were gone never arrived — so
      // an in-flight turn (and its sub-agents) would spin forever. Re-join the
      // active session and REPLAY everything since the last seq we saw, so the
      // missed tool_results / run_parallel result / turn_ended land and the stuck
      // Tasks settle. (since=lastSeq → only the gap, no duplicates.) Mirrors the
      // old CLI's connectDaemonEvents join+replay.
      if (reconnect && lastJoined) {
        joined.clear()
        joined.add(lastJoined)
        replaying = true // events until replay_done are HISTORICAL, not live progress
        socket!.emit("join_session", { session_id: lastJoined })
        socket!.emit("replay", { session_id: lastJoined, since: lastSeqOf.get(lastJoined) ?? 0 })
        dlog(`socket REJOIN+REPLAY ${lastJoined.slice(0, 8)} since=${lastSeqOf.get(lastJoined) ?? 0}`)
        // DEAD-TURN watchdog : if the daemon RESTARTED mid-turn, the turn died with
        // no turn_ended to replay → it would hang forever. A transient drop instead
        // keeps the daemon alive and the turn keeps streaming LIVE events after the
        // replay catches up. So : after a grace window, if a turn is still open AND
        // no LIVE event arrived since reconnect (only historical replayed ones), the
        // turn is dead → settle it. Keying on lastLiveEventAt (not lastEventAt) is
        // what makes replayed events stop masking a dead turn.
        const sid = lastJoined
        const reconnectAt = Date.now()
        setTimeout(() => {
          if (turns.has(sid) && lastLiveEventAt < reconnectAt) {
            dlog(`dead-turn watchdog : ${sid.slice(0, 8)} no LIVE event since reconnect → interrupt`)
            interruptTurn(sid)
          }
        }, 10000)
      }
    })
    // The daemon ends a replay with `replay_done` ; from here on, events are LIVE
    // again. If the turn was dead (daemon crash mid-turn), nothing live follows and
    // the watchdog above settles it ; if the daemon is healthy, live events resume
    // and keep the turn alive.
    socket.on("replay_done", () => {
      replaying = false
      dlog(`socket REPLAY_DONE → live again`)
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
      lastEventAt = Date.now()
      if (!replaying) lastLiveEventAt = Date.now() // only genuine live progress, not replay backfill
      // Track the highest seq per session so a reconnect replay resumes from the
      // exact gap (no re-processing of events we already applied).
      if (typeof env.seq === "number" && env.session_id) {
        if (env.seq > (lastSeqOf.get(env.session_id) ?? 0)) lastSeqOf.set(env.session_id, env.seq)
      }
      try {
        translate(env)
      } catch (e: any) {
        dlog(`translate THREW ${e?.message ?? e}`)
      }
    })
  }
  const joinSession = (sid: string) => {
    ensureSocket()
    lastJoined = sid // active session → re-joined on reconnect
    if (joined.has(sid)) return
    joined.add(sid)
    dlog(`joinSession ${sid.slice(0, 8)} (connected=${socket?.connected})`)
    socket!.emit("join_session", { session_id: sid })
  }

  // /connect (DialogDigitornConnect) calls applyDigitornCredentials after a
  // successful sign-in. We mutate the live cfg (every daemonFetch reads cfg.token
  // at call time, so REST picks it up immediately) and, if the socket is already
  // up, reconnect it with the fresh bearer so the /events stream is authed too.
  reloginHook = (token: string, userID: string) => {
    cfg.token = token || cfg.token
    if (userID) cfg.userID = userID
    if (socket) {
      socket.auth = { token: cfg.token, user_id: cfg.userID }
      socket.disconnect()
      socket.connect()
    }
  }

  return (async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const req = new Request(input as RequestInfo, init)
    const path = new URL(req.url).pathname
    const method = req.method.toUpperCase()
    if (DBG && path !== "/event") dlog(`fetch ${method} ${path}`)

    // @ file completions : opencode's composer fuzzy-finds workdir files via
    // GET /find/file?query=&limit= and expects a string[] of relative paths.
    if (path === "/find/file" && method === "GET") {
      const u = new URL(req.url)
      const limit = Math.max(1, Math.min(200, Number(u.searchParams.get("limit")) || 50))
      return jsonRes(findFiles(dir, u.searchParams.get("query") ?? "", limit))
    }

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
          for (const sid of workBySession.keys()) send(wrap({ type: "digitorn.work", properties: workProps(sid) }))
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
    // Gateway model catalog for the /models dialog, grouped DYNAMICALLY by the
    // gateway's `categories` (owned_by is becoming uniformly "digitorn", so we
    // don't group on it). A model with no category defaults to "free". Read-only:
    // it does NOT touch opencode's provider/model store (which destabilised it).
    if (path === "/digitorn/models" && method === "GET") {
      const { groups, error } = await loadGatewayCatalog()
      // `current` only highlights the active model — never let it block the list.
      const current = error
        ? ""
        : (await Promise.race([
            loadModel(currentApp).catch(() => ""),
            new Promise<string>((r) => setTimeout(() => r(""), 1500)),
          ])) || ""
      return jsonRes({ groups, current, error })
    }
    // Per-agent model switching. GET merges the daemon's per-agent state with the
    // gateway catalog (skipped for BYOK, which switches within the declared list).
    if (path === "/digitorn/session-model" && method === "GET") {
      const sid = new URL(req.url).searchParams.get("session") ?? ""
      if (!sid) return jsonRes({ agents: [], error: "no active session" })
      try {
        const state = await daemonFetch<any>(
          cfg,
          `/api/apps/${encodeURIComponent(currentApp)}/sessions/${encodeURIComponent(sid)}/model`,
        )
        const byok = !!state?.byok
        const agents = Array.isArray(state?.agents) ? state.agents : []
        const catalog: Record<string, CatModel[]> = {}
        let error: string | undefined
        if (!byok) {
          const cat = await loadGatewayCatalog()
          error = cat.error
          for (const g of cat.groups) catalog[g.category] = g.models
        }
        return jsonRes({ byok, entry: state?.entry ?? "", agents, catalog, error })
      } catch (e: any) {
        return jsonRes({ agents: [], error: String(e?.message ?? e) })
      }
    }
    if (path === "/digitorn/session-model" && method === "PUT") {
      const sid = new URL(req.url).searchParams.get("session") ?? ""
      if (!sid) return jsonRes({ error: "no active session" }, 400)
      const b = (await req.json().catch(() => ({}))) as Record<string, any>
      // Raw fetch so the daemon's error body + status pass through to the dialog.
      try {
        const r = await fetch(
          `${cfg.url}/api/apps/${encodeURIComponent(currentApp)}/sessions/${encodeURIComponent(sid)}/model`,
          {
            method: "PUT",
            headers: { authorization: `Bearer ${cfg.token}`, "content-type": "application/json" },
            body: JSON.stringify({ agent: String(b?.agent ?? ""), model: String(b?.model ?? "") }),
          },
        )
        const txt = await r.text()
        return new Response(txt || "{}", { status: r.status, headers: { "content-type": "application/json" } })
      } catch (e: any) {
        return jsonRes({ error: String(e?.message ?? e) }, 502)
      }
    }
    // Skills CRUD for the /skill dialog, proxied to the daemon's
    // /api/apps/{app}/skills. GET is normalized (daemon JSON casing varies);
    // writes pass through and surface the daemon's error (name conflict /
    // authoring disabled) so the dialog can show it.
    if (path === "/digitorn/skills" && method === "GET") {
      if (DBG) dlog(`/digitorn/skills GET entered app=${currentApp}`)
      try {
        const resp = await fetch(`${cfg.url}/api/apps/${encodeURIComponent(currentApp)}/skills`, {
          headers: { authorization: `Bearer ${cfg.token}` },
          signal: AbortSignal.timeout(6000),
        })
        if (!resp.ok) throw new Error(`HTTP ${resp.status}`)
        const r: any = await resp.json()
        const appSkills = (r?.app_skills ?? []).map((s: any) => ({
          command: String(s?.command ?? s?.Command ?? ""),
          description: String(s?.description ?? s?.Description ?? ""),
        }))
        const userSkills = (r?.user_skills ?? []).map((s: any) => ({
          id: String(s?.id ?? s?.ID ?? ""),
          name: String(s?.name ?? s?.Name ?? ""),
          description: String(s?.description ?? s?.Description ?? ""),
          instructions: String(s?.instructions ?? s?.Instructions ?? ""),
        }))
        if (DBG) dlog(`/digitorn/skills -> app=${appSkills.length} user=${userSkills.length} allow=${!!(r?.allow_user_skills ?? r?.allowUserSkills)}`)
        return jsonRes({ appSkills, userSkills, allow: !!(r?.allow_user_skills ?? r?.allowUserSkills) })
      } catch (e: any) {
        dlog(`/digitorn/skills FAILED ${String(e?.message ?? e)}`)
        return jsonRes({ appSkills: [], userSkills: [], allow: false, error: String(e?.message ?? e) })
      }
    }
    if (path === "/digitorn/skills" && method === "POST") {
      const b = (await req.json().catch(() => ({}))) as Record<string, any>
      try {
        const r = await daemonFetch(cfg, `/api/apps/${encodeURIComponent(currentApp)}/skills`, {
          method: "POST",
          body: JSON.stringify({ name: b?.name, description: b?.description, instructions: b?.instructions }),
        })
        return jsonRes(r as any)
      } catch (e: any) {
        return jsonRes({ error: String(e?.message ?? e) }, 400)
      }
    }
    const skillOne = path.match(/^\/digitorn\/skills\/([^/]+)$/)
    if (skillOne && method === "PATCH") {
      const id = decodeURIComponent(skillOne[1])
      const b = (await req.json().catch(() => ({}))) as Record<string, any>
      try {
        const r = await daemonFetch(cfg, `/api/apps/${encodeURIComponent(currentApp)}/skills/${encodeURIComponent(id)}`, {
          method: "PATCH",
          body: JSON.stringify(b),
        })
        return jsonRes(r as any)
      } catch (e: any) {
        return jsonRes({ error: String(e?.message ?? e) }, 400)
      }
    }
    if (skillOne && method === "DELETE") {
      const id = decodeURIComponent(skillOne[1])
      try {
        await daemonFetch(cfg, `/api/apps/${encodeURIComponent(currentApp)}/skills/${encodeURIComponent(id)}`, {
          method: "DELETE",
        })
        return jsonRes({ deleted: true })
      } catch (e: any) {
        return jsonRes({ error: String(e?.message ?? e) }, 400)
      }
    }
    // Workspace file review : approve (commit to the shadow-git baseline) or
    // reject (revert) the agent's pending changes — file-level, or all at once.
    // After each, the daemon pokes FileChanged → workspace_changes event → our
    // translate re-emits session.diff, so the files panel + /diff self-refresh.
    const wsAction = path.match(/^\/digitorn\/workspace\/(approve-all|reject-all|approve|reject)$/)
    if (wsAction && method === "POST") {
      const b = (await req.json().catch(() => ({}))) as Record<string, any>
      const sid = String(b?.session ?? "")
      if (!sid) return jsonRes({ error: "no session" }, 400)
      const base = `/api/apps/${encodeURIComponent(currentApp)}/sessions/${encodeURIComponent(sid)}/workspace`
      try {
        if (wsAction[1] === "approve-all") {
          return jsonRes((await daemonFetch(cfg, `${base}/files/approve-all`, { method: "POST", body: "{}" })) as any)
        }
        if (wsAction[1] === "reject-all") {
          // No daemon reject-all : revert every pending path in one reject call.
          const ch = await daemonFetch<{ files?: Record<string, any>[] }>(cfg, `${base}/changes`)
          const paths = (ch.files ?? []).map((f) => String(f.path ?? "")).filter(Boolean)
          if (paths.length === 0) return jsonRes({ rejected: 0 })
          return jsonRes((await daemonFetch(cfg, `${base}/files/reject`, { method: "POST", body: JSON.stringify({ paths }) })) as any)
        }
        const endpoint = wsAction[1] === "approve" ? "files/approve" : "files/reject"
        return jsonRes(
          (await daemonFetch(cfg, `${base}/${endpoint}`, {
            method: "POST",
            body: JSON.stringify({ paths: [String(b?.path ?? "")] }),
          })) as any,
        )
      } catch (e: any) {
        return jsonRes({ error: String(e?.message ?? e) }, 400)
      }
    }
    // Per-hunk review : parse a file's unified diff into hunks (web-identical
    // hashes) so the dialog can list them; the daemon's /workspace/diff is the
    // SAME source the hunk hashes are computed from, so they match on approve.
    if (path === "/digitorn/workspace/hunks" && method === "GET") {
      const u = new URL(req.url)
      const sid = u.searchParams.get("session") ?? ""
      const fp = u.searchParams.get("path") ?? ""
      if (!sid || !fp) return jsonRes({ hunks: [] })
      try {
        const r = await daemonFetch<{ unified?: string }>(
          cfg,
          `/api/apps/${encodeURIComponent(currentApp)}/sessions/${encodeURIComponent(sid)}/workspace/diff?path=${encodeURIComponent(fp)}`,
        )
        return jsonRes({ hunks: parseHunks(String(r.unified ?? "")) })
      } catch (e: any) {
        return jsonRes({ hunks: [], error: String(e?.message ?? e) })
      }
    }
    // Per-hunk approve (commit baseline + selected hunk) / reject (revert only the
    // selected hunk). Body : {session, path, hunks:[hash]}.
    const wsHunkAction = path.match(/^\/digitorn\/workspace\/(approve-hunks|reject-hunks)$/)
    if (wsHunkAction && method === "POST") {
      const b = (await req.json().catch(() => ({}))) as Record<string, any>
      const sid = String(b?.session ?? "")
      if (!sid) return jsonRes({ error: "no session" }, 400)
      const endpoint = wsHunkAction[1] === "approve-hunks" ? "files/approve-hunks" : "files/reject-hunks"
      try {
        return jsonRes(
          (await daemonFetch(
            cfg,
            `/api/apps/${encodeURIComponent(currentApp)}/sessions/${encodeURIComponent(sid)}/workspace/${endpoint}`,
            {
              method: "POST",
              body: JSON.stringify({ path: String(b?.path ?? ""), hunks: Array.isArray(b?.hunks) ? b.hunks : [] }),
            },
          )) as any,
        )
      } catch (e: any) {
        return jsonRes({ error: String(e?.message ?? e) }, 400)
      }
    }
    // Aggregated status for the /status dialog: app + daemon + context + model +
    // modules, assembled from loadApps + connState + ctxBySession + the app
    // manifest (parsed defensively — daemon JSON casing varies).
    if (path === "/digitorn/status" && method === "GET") {
      const sid = new URL(req.url).searchParams.get("session") ?? ""
      const apps = await loadApps().catch(() => [] as Awaited<ReturnType<typeof loadApps>>)
      const cur = apps.find((a) => a.id === currentApp)
      const c = ctxFor(sid)
      const percent = c.window > 0 ? Math.round((c.used / c.window) * 100) : 0
      let version = ""
      let model = ""
      const modules = new Set<string>()
      try {
        const m: any = await daemonFetch(cfg, `/api/apps/${encodeURIComponent(currentApp)}/manifest`)
        const appMeta = m?.App ?? m?.app ?? {}
        version = String(appMeta.Version ?? appMeta.version ?? m?.Version ?? m?.version ?? "")
        const tools = m?.Tools ?? m?.tools ?? {}
        for (const k of Object.keys(tools.Modules ?? tools.modules ?? m?.Modules ?? m?.modules ?? {})) modules.add(k)
        const caps = tools.Capabilities ?? tools.capabilities ?? m?.Capabilities ?? m?.capabilities ?? {}
        for (const g of caps.Grant ?? caps.grant ?? []) {
          const name = g?.Module ?? g?.module
          if (name) modules.add(String(name))
        }
        const agents = m?.Agents ?? m?.agents ?? []
        const brain = agents[0]?.Brain ?? agents[0]?.brain ?? {}
        model = String(brain.Model ?? brain.model ?? "")
      } catch {}
      return jsonRes({
        app: { id: currentApp, name: cur?.name ?? currentApp, version, color: cur?.color ?? "" },
        daemon: { state: connState, url: cfg.url },
        context: { tokens: c.used, window: c.window, percent },
        model,
        modules: [...modules],
      })
    }
    // One-shot SNAPSHOT of the current turn's working state (live tokens / exact /
    // compacting). Live updates arrive via the pushed digitorn.work event.
    if (path === "/digitorn/work" && method === "GET") {
      const sid = new URL(req.url).searchParams.get("session") ?? ""
      return jsonRes(workProps(sid))
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
        void broadcastApp() // update the home active-app indicator (push)
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
      // /use_skill <name> <message> : the picker prefilled this prefix. Strip it,
      // pull the skill out, and send the rest as the message WITH `skill` so the
      // daemon injects the skill as a forced directive (engine.injectSkillDirective).
      let content = text
      let skill = ""
      const useSkill = text.match(/^\/use_skill\s+(\S+)\s*([\s\S]*)$/)
      if (useSkill) {
        skill = useSkill[1].startsWith("/") ? useSkill[1] : "/" + useSkill[1]
        content = useSkill[2].trim()
      }
      dlog(`PROMPT route ${path} sid=${sid.slice(0, 8)} textLen=${content.length}${skill ? ` skill=${skill}` : ""}`)
      joinSession(sid)
      // Client-side queue : if a turn is already running, DON'T start a 2nd one —
      // hold this prompt and render it as a synthetic QUEUED message (a seq far
      // above any daemon seq → it sorts at the bottom yet above the in-flight
      // assistant, so opencode shows its QUEUED badge). flushQueue sends it for
      // real (removing the synthetic) the instant the session next goes idle.
      if (busy.has(sid)) {
        const list = promptQueue.get(sid) ?? []
        const seq = (lastSeqOf.get(sid) ?? 0) + 1_000_000 + list.length
        const msgID = `${sid}:m:${padNum(seq)}`
        list.push({ content, mode: String(body?.agent ?? ""), skill, msgID })
        promptQueue.set(sid, list)
        emit(
          wrap({
            type: "message.updated",
            properties: {
              sessionID: sid,
              info: {
                id: msgID,
                sessionID: sid,
                role: "user",
                time: { created: Date.now() },
                agent: "build",
                model: { providerID: "digitorn", modelID: "build" },
              },
            },
          }),
        )
        partUpdated(sid, { id: `${msgID}:p${padNum(0, 4)}`, sessionID: sid, messageID: msgID, type: "text", text: content })
        return jsonRes({})
      }
      // Immediate feedback : mark the session busy the instant the user sends, so
      // the working indicator shows right away instead of after the daemon's
      // turn_started (which can lag, or never come if the call fails up front) —
      // "I sent it and nothing happened" was exactly that gap. Cleared by
      // turn_ended / error / session_interrupted.
      emitStatus(sid, "busy")
      try {
        await daemonFetch(cfg, `/api/apps/${app}/sessions/${encodeURIComponent(sid)}/messages`, {
          method: "POST",
          // opencode sends the picked agent name — which IS our mode (build/plan/…).
          body: JSON.stringify({ content, role: "user", mode: String(body?.agent ?? ""), ...(skill ? { skill } : {}) }),
        })
      } catch (e: any) {
        // The send itself failed (daemon down, 4xx, …) → there'll be no turn at all.
        // Route it through the SAME error path (surfaces the banner + flips idle) so
        // the spinner doesn't spin forever on a message that never left.
        translate({ type: "error", session_id: sid, payload: { error: String(e?.message ?? e), source: "send" } })
      }
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
    // opencode's DialogForkFromTimeline → sdk.client.session.fork → POST
    // /session/{id}/fork, then it navigates to forked.data.id. Without this handler
    // the call fell through to the empty catch-all, data.id was undefined, and the
    // session route rendered a BLACK screen. The daemon clones the durable log under
    // a new id and returns {session_id:newSid,…}, so toOcSession yields a valid
    // Session to navigate to. "Full session" sends no messageID → full clone. The
    // per-message "fork from here" sends messageID ; our ids encode the daemon seq
    // (`${sid}:m:${padNum(seq)}`), so we extract it and pass before_seq → the daemon
    // clones only events strictly before that message (rewind-to-here).
    const forkPost = path.match(/^\/session\/([^/]+)\/fork$/)
    if (forkPost && method === "POST") {
      const sid = decodeURIComponent(forkPost[1])
      const body = (await req.json().catch(() => ({}))) as Record<string, any>
      const sm = String(body?.messageID ?? "").match(/:m:(\d+)$/)
      const q = sm ? `?before_seq=${parseInt(sm[1], 10)}` : ""
      const r = (await daemonFetch(cfg, `/api/apps/${app}/sessions/${encodeURIComponent(sid)}/fork${q}`, {
        method: "POST",
      })) as Record<string, any>
      return jsonRes(toOcSession(r, dir))
    }
    // /rename : opencode's DialogSessionRename → sdk.client.session.update → PATCH
    // /session/{id} {title}. Persist it on the daemon (durable rename) and emit a
    // FULL session.updated so the new title shows in the list + header (sync's
    // reconcile REPLACES the entry, so a partial object would wipe other fields).
    const renamePatch = path.match(/^\/session\/([^/]+)$/)
    if (renamePatch && method === "PATCH") {
      const sid = decodeURIComponent(renamePatch[1])
      const body = (await req.json().catch(() => ({}))) as Record<string, any>
      const title = String(body?.title ?? "").trim()
      if (!title) return jsonRes(toOcSession({ session_id: sid }, dir))
      try {
        const r = (await daemonFetch(cfg, `/api/apps/${app}/sessions/${encodeURIComponent(sid)}`, {
          method: "PATCH",
          body: JSON.stringify({ title }),
        })) as Record<string, any>
        const sess = toOcSession(r, dir)
        emit(wrap({ type: "session.updated", properties: { info: sess } }))
        return jsonRes(sess)
      } catch {
        return jsonRes(toOcSession({ session_id: sid, title }, dir))
      }
    }
    // Slash commands (DIGITORN_COMMANDS) → existing daemon session endpoints.
    // opencode fires session.command as void and relies on events/toasts for
    // feedback. compact → /compact (emits compaction events); fork → /fork then
    // navigate to the new session; export → /export written to the workdir.
    const cmdPost = path.match(/^\/session\/([^/]+)\/command$/)
    if (cmdPost && method === "POST") {
      const sid = decodeURIComponent(cmdPost[1])
      const body = (await req.json().catch(() => ({}))) as Record<string, any>
      const command = String(body?.command ?? "")
      const base = `/api/apps/${app}/sessions/${encodeURIComponent(sid)}`
      const toast = (message: string, variant: "info" | "success" | "warning" | "error" = "success") =>
        emit(wrap({ type: "tui.toast.show", properties: { message, variant } }))
      try {
        if (command === "compact") {
          const r = (await daemonFetch(cfg, `${base}/compact`, { method: "POST" })) as Record<string, any>
          toast(r?.events_compacted ? `Compacted ${r.events_compacted} events` : "Conversation compacted")
        } else if (command === "fork") {
          const r = (await daemonFetch(cfg, `${base}/fork`, { method: "POST" })) as Record<string, any>
          const newId = String(r?.new_session_id ?? r?.session_id ?? "")
          toast(`Forked → ${r?.title ?? newId}`)
          if (newId) emit(wrap({ type: "tui.session.select", properties: { sessionID: newId } }))
        } else if (command === "export") {
          const r = (await daemonFetch(cfg, `${base}/export?format=markdown`)) as Record<string, any>
          const fname = String(r?.filename ?? `export_${sid}.md`)
          writeFileSync(join(dir, fname), String(r?.content ?? ""))
          toast(`Exported → ${fname}`)
        } else {
          toast(`Unknown command: /${command}`, "warning")
        }
      } catch (e: any) {
        toast(`/${command} failed: ${String(e?.message ?? e)}`, "error")
      }
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
    // string[][] (per question → selected labels). We re-encode for the daemon's
    // /approve `reason` per the question shape : a form → a JSON object keyed by
    // field name (with type coercion), a multi-select → comma-joined (the daemon
    // splits on comma), text/single/content → the selected/typed value.
    const qReply = path.match(/^\/question\/([^/]+)\/(reply|reject)$/)
    if (qReply && method === "POST") {
      const reqID = decodeURIComponent(qReply[1])
      const reject = qReply[2] === "reject"
      const body = (await req.json().catch(() => ({}))) as Record<string, any>
      const answers: string[][] = Array.isArray(body?.answers)
        ? body.answers.map((a: any) => (Array.isArray(a) ? a.map(String) : [String(a ?? "")]))
        : []
      const answer = encodeQuestionReply(questionShapes.get(reqID), answers)
      questionShapes.delete(reqID)
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
      emitSessionDiff(sid) // seed the files panel with existing pending changes
      try {
        const r = await daemonFetch<{ messages?: Record<string, any>[] }>(
          cfg,
          `/api/apps/${app}/sessions/${encodeURIComponent(sid)}/history?limit=2000`,
        )
        const msgs = r.messages ?? []
        // Replay any events emitted AFTER this history snapshot. Covers the
        // create-on-home → send → open-chat race : the first turn can stream (and
        // even finish) before we joined the room, so its events would be lost and
        // the message would sit with no reply. Replay since the history's last seq
        // fills exactly that gap without re-feeding the whole session.
        const maxSeq = msgs.reduce((mx, m) => Math.max(mx, Number(m?.seq ?? 0)), lastSeqOf.get(sid) ?? 0)
        lastSeqOf.set(sid, maxSeq)
        socket?.emit("replay", { session_id: sid, since: maxSeq })
        dlog(`open-replay ${sid.slice(0, 8)} since=${maxSeq}`)
        return jsonRes(toOcMessages(sid, msgs, dir))
      } catch {
        return jsonRes([])
      }
    }
    // Per-session todo list (maintained live from todo_* events). diff stays [].
    const todoGet = path.match(/^\/session\/([^/]+)\/todo$/)
    if (todoGet && method === "GET") {
      return jsonRes(todosFor(decodeURIComponent(todoGet[1])))
    }
    // Session-scoped diff (opencode's "last-turn" source + the sidebar files
    // panel's session.diff store). Maps the daemon's shadow-git pending changes
    // to SnapshotFileDiff[]. messageID is ignored : the daemon tracks one live
    // change set per session, not per message.
    const diffOne = path.match(/^\/session\/([^/]+)\/diff$/)
    if (method === "GET" && diffOne) {
      const sid = decodeURIComponent(diffOne[1])
      try {
        return jsonRes(await fetchSessionDiff(sid))
      } catch {
        return jsonRes([])
      }
    }
    // VCS "git" source of the /diff viewer (default mode). opencode calls this
    // project-wide with no session, so we resolve to the active session
    // (lastJoined) — our change tracking is per-session shadow git, not a global
    // working tree. Returns the same VcsFileDiff[] shape as session.diff.
    if (method === "GET" && path === "/vcs/diff") {
      try {
        return jsonRes(await fetchSessionDiff(lastJoined))
      } catch {
        return jsonRes([])
      }
    }
    const sessOne = path.match(/^\/session\/([^/]+)$/)
    if (sessOne && sessOne[1] !== "status" && method === "GET") {
      // A sub-agent's CHILD session ("<root>::agent::<run>") is NOT a first-class
      // daemon session (the daemon 404s its /history). Its transcript lives in the
      // live store : registered via session.updated, streamed via the ancestor
      // fan-out. If we returned a session here, opencode's sync.session.sync would
      // succeed on session.get, then fetch the (empty) child history and OVERWRITE
      // that live data with [] — wiping every finished sub-agent's tool activity
      // (and the drilled-in transcript). 404 → sync.session.sync rejects on the
      // throwOnError session.get and never reaches the wiping setStore; the Task's
      // nested ↳ and the drill-in both read the intact store.
      if (decodeURIComponent(sessOne[1]).includes("::agent::")) return jsonRes({ error: "sub_session" }, 404)
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
        // ConsoleState : opencode reconciles this into sync.data.console_state.
        // Must carry consoleManagedProviders (the model/provider dialogs call
        // .has/.includes on it) — a bare {} makes it undefined and crashes /models.
        case "/experimental/console":
          return jsonRes({ consoleManagedProviders: [], switchableOrgCount: 0 })
        case "/config/providers": // populates data.provider + provider_default
          await applyAppModel()
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
          await applyAppModel()
          return jsonRes({ all: [dgProvider], default: dgDefault, connected: ["digitorn"] })
        case "/provider/auth":
          return jsonRes({})
        case "/agent": {
          // app.agents → our digitorn MODES as opencode primary agents, so Tab
          // cycles them (build/plan/…) exactly like the old CLI's mode picker.
          const modes = await loadModes(currentApp)
          // native:false → the mode picker shows each mode's real description (not
          // the literal word "native"). mode:"primary" keeps them Tab-cyclable.
          if (modes.length)
            return jsonRes(modes.map((m) => ({ name: m.id, description: m.description || m.label, mode: "primary", native: false, permission: {}, options: {} })))
          return jsonRes([{ name: "build", mode: "primary", native: false, permission: {}, options: {} }])
        }
        case "/session":
          return jsonRes([]) // ÉTAPE 1 : real session list
        case "/session/status":
          return jsonRes({})
        // Slash commands backed by daemon session endpoints. opencode lists these
        // (sync.data.command) and runs them via POST /session/{id}/command, which
        // we dispatch below. template/hints are required by the Command type.
        case "/command":
          return jsonRes(DIGITORN_COMMANDS)
        // Non-blocking boot stores : arrays vs objects matter for reconcile().
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

// parseHunks splits one file's unified diff into hunks, computing each hunk's
// stable 12-char hash IDENTICALLY to the daemon (internal/gitrepo/hunks.go) so a
// per-hunk approve/reject references the exact hunk byte-for-byte:
//   hash = sha256(header + "\n" + body.join("\n")).hex()[:12]
// header = the full "@@ … @@[ ctx]" line (only when it matches the @@ regex);
// body = the content lines (" "/"-"/"+"), kept verbatim with their prefix. Lines
// before the first @@ (the diff/index/file markers) are ignored — exactly as the
// daemon does, or the hash would diverge and the daemon would find nothing.
const HUNK_HEADER_RE = /^@@ -\d+(?:,\d+)? \+\d+(?:,\d+)? @@/
type DiffHunk = { hash: string; header: string; additions: number; deletions: number }
function parseHunks(unified: string): DiffHunk[] {
  const out: DiffHunk[] = []
  let header = ""
  let body: string[] = []
  let adds = 0
  let dels = 0
  const flush = () => {
    if (!header) return
    const hash = createHash("sha256")
      .update(header + "\n" + body.join("\n"))
      .digest("hex")
      .slice(0, 12)
    out.push({ hash, header, additions: adds, deletions: dels })
    header = ""
    body = []
    adds = 0
    dels = 0
  }
  for (const line of unified.split("\n")) {
    if (line.startsWith("@@")) {
      flush()
      if (HUNK_HEADER_RE.test(line)) header = line
      continue
    }
    if (header && line.length > 0 && (line[0] === " " || line[0] === "-" || line[0] === "+")) {
      body.push(line)
      if (line[0] === "+") adds++
      else if (line[0] === "-") dels++
    }
  }
  flush()
  return out
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
    const rawContent = String(m.content ?? "")
    // The daemon's interruption marker is LLM-only — strip it for display and flag
    // the message interrupted instead (opencode shows a subtle "· interrupted").
    const interrupted = role === "assistant" && hadInterruptMarker(rawContent)
    const content = interrupted ? stripInterruptMarker(rawContent) : rawContent
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
          // Marker stripped → show opencode's subtle "· interrupted" instead.
          ...(interrupted ? { error: { name: "MessageAbortedError", data: { message: "Interrupted" } } } : {}),
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
