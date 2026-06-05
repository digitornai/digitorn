// Live chat test : create a session, open the /event stream, send a prompt,
// collect translated events. Proves ÉTAPE 3 (send + Socket.IO→GlobalEvent).
import { digitornFetch, digitornConfig, daemonFetch } from "./src/cli/cmd/tui/context/digitorn"

const cfg = digitornConfig()
const f = digitornFetch(cfg)
const app = encodeURIComponent(cfg.app)

const created: any = await daemonFetch(cfg, `/api/apps/${app}/sessions`, {
  method: "POST",
  body: JSON.stringify({ title: "adapter live test", workdir: process.cwd() }),
})
const sid = created.session_id ?? created.id
console.log("session:", sid)

// Open the /event SSE and read it.
const evRes = await f(`${cfg.url}/event` as any)
const reader = (evRes.body as ReadableStream<Uint8Array>).getReader()
const dec = new TextDecoder()
const events: any[] = []
let buf = ""
;(async () => {
  while (true) {
    const { done, value } = await reader.read()
    if (done) break
    buf += dec.decode(value, { stream: true })
    let i: number
    while ((i = buf.indexOf("\n\n")) >= 0) {
      const chunk = buf.slice(0, i)
      buf = buf.slice(i + 2)
      const line = chunk.split("\n").find((l) => l.startsWith("data: "))
      if (line) {
        try {
          events.push(JSON.parse(line.slice(6)))
        } catch {}
      }
    }
  }
})().catch(() => {})

await new Promise((r) => setTimeout(r, 500))
await f(`${cfg.url}/session/${sid}/prompt_async` as any, {
  method: "POST",
  body: JSON.stringify({ parts: [{ type: "text", text: "Réponds juste le mot: bonjour" }] }),
})
console.log("prompt sent, listening 25s…")

await new Promise((r) => setTimeout(r, 25000))
await reader.cancel().catch(() => {})

const types = events.map((e) => e?.payload?.type)
console.log("total events:", events.length)
console.log("event types:", JSON.stringify([...new Set(types)]))
const lastPart = [...events].reverse().find((e) => e?.payload?.type === "message.part.updated")
console.log("last streamed text:", JSON.stringify(lastPart?.payload?.properties?.part?.text?.slice(0, 120)))
const updated = events.filter((e) => e?.payload?.type === "message.updated").length
console.log("message.updated count:", updated)
process.exit(0)
