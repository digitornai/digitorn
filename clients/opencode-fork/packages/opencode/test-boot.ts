// Boot harness : replays sync.tsx's blocking boot calls against digitornFetch,
// using the REAL opencode SDK client, WITHOUT loading the native TUI. Surfaces
// shape mismatches (the crashes) so the adapter can be fixed without a live TUI.
import { createOpencodeClient } from "@opencode-ai/sdk/v2"
import { digitornFetch, digitornConfig, daemonFetch } from "./src/cli/cmd/tui/context/digitorn"

const cfg = digitornConfig()
const sdk: any = createOpencodeClient({ baseUrl: cfg.url, fetch: digitornFetch(cfg) })
const c = sdk // the OpencodeClient itself exposes .config/.provider/.app getters
console.log("has config getter:", typeof sdk.config, " provider:", typeof sdk.provider, " app:", typeof sdk.app)

async function call(name: string, fn: () => Promise<any>): Promise<any> {
  try {
    const r = await fn()
    const d = r?.data ?? r
    console.log(`OK   ${name} →`, JSON.stringify(d).slice(0, 140))
    return d
  } catch (e) {
    console.log(`FAIL ${name} →`, (e as Error).message)
    return undefined
  }
}

const ws = undefined
const providers = await call("config.providers", () => c.config.providers({ workspace: ws }, { throwOnError: true }))
const providerList = await call("provider.list", () => c.provider.list({ workspace: ws }, { throwOnError: true }))
const agents = await call("app.agents", () => c.app.agents({ workspace: ws }, { throwOnError: true }))
const config = await call("config.get", () => c.config.get({ workspace: ws }, { throwOnError: true }))
await call("path.get", () => c.path.get({ workspace: ws }, { throwOnError: true }))
await call("project.current", () => c.project.current({ workspace: ws }, { throwOnError: true }))
await call("session.list", () => c.session.list({ workspace: ws }, { throwOnError: true }))

// The exact operations the sync layer + use-connected do on the data :
console.log("\n--- downstream ops (the crash points) ---")
try {
  console.log("providers.providers isArray:", Array.isArray(providers?.providers))
  // use-connected.tsx : sync.data.provider.some(...)
  const ok = providers?.providers.some((x: any) => x.id !== "opencode")
  console.log("data.provider.some() OK:", ok)
} catch (e) {
  console.log("CRASH on provider.some:", (e as Error).message)
}
try {
  console.log("provider_next.all isArray:", Array.isArray(providerList?.all))
} catch (e) {
  console.log("CRASH provider_next:", (e as Error).message)
}
console.log("agents isArray:", Array.isArray(agents))

// Non-blocking boot stores (must not throw ; defaults tolerate empties).
console.log("\n--- non-blocking boot calls ---")
await call("command.list", () => c.command.list({ workspace: ws }))
await call("lsp.status", () => c.lsp.status({ workspace: ws }))
await call("mcp.status", () => c.mcp.status({ workspace: ws }))
await call("formatter.status", () => c.formatter.status({ workspace: ws }))
await call("session.status", () => c.session.status({ workspace: ws }))
await call("provider.auth", () => c.provider.auth({ workspace: ws }))
await call("vcs.get", () => c.vcs.get({ workspace: ws }))

// The live event stream : sdk.global.event() must yield our SSE server.connected.
console.log("\n--- event stream (/event SSE) ---")
try {
  const events: any = await sdk.global.event({ sseMaxRetryAttempts: 0 })
  const it = events.stream[Symbol.asyncIterator]()
  const first = (await Promise.race([
    it.next(),
    new Promise((_, rej) => setTimeout(() => rej(new Error("timeout 3s")), 3000)),
  ])) as any
  console.log("first event:", JSON.stringify(first?.value).slice(0, 160))
  it.return?.()
} catch (e) {
  console.log("event stream FAIL:", (e as Error).message)
}

// ÉTAPE 2 : message history of a real session.
console.log("\n--- session.messages (history of first session) ---")
const list: any = await call("session.list2", () => c.session.list({ workspace: ws }, { throwOnError: true }))
const firstId = Array.isArray(list) ? list[0]?.id : undefined
console.log("first session id:", firstId)
if (firstId) {
  const msgs: any = await call(`session.messages`, () =>
    c.session.messages({ sessionID: firstId, limit: 100 }, { throwOnError: true }),
  )
  if (Array.isArray(msgs)) {
    console.log("messages count:", msgs.length)
    if (msgs[0]) console.log("first item shape:", JSON.stringify(msgs[0]).slice(0, 220))
  }
}

// The exact session OPEN sequence (sync.tsx sync()) : get/messages/todo/diff.
console.log("\n--- session OPEN fetches (get/todo/diff) ---")
if (firstId) {
  await call("session.get", () => c.session.get({ sessionID: firstId }, { throwOnError: true }))
  const todo: any = await call("session.todo", () => c.session.todo({ sessionID: firstId }))
  const diff: any = await call("session.diff", () => c.session.diff({ sessionID: firstId }))
  console.log("todo isArray:", Array.isArray(todo), " diff isArray:", Array.isArray(diff))
  try {
    console.log("diff.flatMap OK len:", (diff ?? []).flatMap((x: any) => [x]).length)
  } catch (e) {
    console.log("diff.flatMap CRASH:", (e as Error).message)
  }
}

// DIRECT daemonFetch (bypass SDK) to isolate fetch vs translation.
console.log("\n--- direct daemonFetch history ---")
if (firstId) {
  try {
    const raw: any = await daemonFetch(
      cfg,
      `/api/apps/${encodeURIComponent(cfg.app)}/sessions/${encodeURIComponent(firstId)}/history?limit=5`,
    )
    console.log("cfg.app:", cfg.app, "tokenLen:", cfg.token.length, "user:", cfg.userID.slice(0, 8))
    console.log("direct messages:", (raw?.messages ?? []).length, "first role:", raw?.messages?.[0]?.role)
  } catch (e) {
    console.log("daemonFetch THREW:", (e as Error).message)
  }
}

// DIRECT digitornFetch route (bypass SDK) : isolate route-match vs translation.
console.log("\n--- direct digitornFetch route ---")
if (firstId) {
  const f = digitornFetch(cfg)
  const res = await f(`${cfg.url}/session/${firstId}/message` as any)
  const data: any = await res.json()
  console.log("route status:", res.status, "items:", Array.isArray(data) ? data.length : `not-array(${typeof data})`)
  if (Array.isArray(data) && data[0]) console.log("first item:", JSON.stringify(data[0]).slice(0, 200))
}

console.log("\nboot harness done")
process.exit(0)
