// Try to trigger the agent's todo tool and verify todo_* → todo.updated.
import { digitornFetch, digitornConfig, daemonFetch, connectDaemonEvents } from "./src/cli/cmd/tui/context/digitorn"

const cfg = digitornConfig()
const f = digitornFetch(cfg)
const app = encodeURIComponent(cfg.app)

const created: any = await daemonFetch(cfg, `/api/apps/${app}/sessions`, {
  method: "POST",
  body: JSON.stringify({ title: "todo test", workdir: process.cwd() }),
})
const sid = created.session_id ?? created.id
console.log("session:", sid)

// Raw daemon events (to see if todo_added/updated even fire).
const rawTypes: string[] = []
connectDaemonEvents(cfg, sid, (env) => rawTypes.push(env.type), () => {})

// Translated GlobalEvents through the real adapter.
const todoEvents: any[] = []
const evRes = await f(`${cfg.url}/event` as any)
const reader = (evRes.body as ReadableStream<Uint8Array>).getReader()
const dec = new TextDecoder()
let buf = ""
;(async () => {
  while (true) {
    const { done, value } = await reader.read()
    if (done) break
    buf += dec.decode(value, { stream: true })
    let i: number
    while ((i = buf.indexOf("\n\n")) >= 0) {
      const line = buf.slice(0, i).split("\n").find((l) => l.startsWith("data: "))
      buf = buf.slice(i + 2)
      if (line) try { const e = JSON.parse(line.slice(6)); if (e?.payload?.type === "todo.updated") todoEvents.push(e.payload.properties) } catch {}
    }
  }
})().catch(() => {})

await new Promise((r) => setTimeout(r, 500))
await f(`${cfg.url}/session/${sid}/prompt_async` as any, {
  method: "POST",
  body: JSON.stringify({ parts: [{ type: "text", text: "Crée une liste de tâches (todo list) avec au moins 3 étapes pour ajouter une fonctionnalité de login, en utilisant ton outil de gestion de tâches." }] }),
})
console.log("prompt sent, 40s…")
await new Promise((r) => setTimeout(r, 40000))
await reader.cancel().catch(() => {})

console.log("raw daemon todo events:", rawTypes.filter((t) => t.startsWith("todo")).length, "/", rawTypes.length, "total")
console.log("translated todo.updated count:", todoEvents.length)
if (todoEvents.length) console.log("last todo list:", JSON.stringify(todoEvents[todoEvents.length - 1].todos))
const final: any = await f(`${cfg.url}/session/${sid}/todo` as any)
console.log("/session/{id}/todo route →", JSON.stringify(await final.json()))
process.exit(0)
