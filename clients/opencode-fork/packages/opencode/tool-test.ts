// Étape 4 verification : drive a tool-forcing turn through the REAL adapter
// (digitornFetch /event + prompt) on claude-code, collect the GlobalEvents, and
// assert the assistant message carries ordered parts (reasoning/text/tool) with
// the tool part settling to completed — i.e. opencode renders a real ToolPart.
import { digitornFetch, digitornConfig, daemonFetch } from "./src/cli/cmd/tui/context/digitorn"

const cfg = digitornConfig()
const f = digitornFetch(cfg)
const app = encodeURIComponent(cfg.app)

const created: any = await daemonFetch(cfg, `/api/apps/${app}/sessions`, {
  method: "POST",
  body: JSON.stringify({ title: "tool part test", workdir: process.cwd() }),
})
const sid = created.session_id ?? created.id
console.log("session:", sid, "app:", cfg.app)

const events: any[] = []
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

await new Promise((r) => setTimeout(r, 400))
await f(`${cfg.url}/session/${sid}/prompt_async` as any, {
  method: "POST",
  body: JSON.stringify({ parts: [{ type: "text", text: "Liste les fichiers du dossier courant avec l'outil approprié, puis dis combien il y en a." }] }),
})
console.log("prompt sent, listening 30s…")
await new Promise((r) => setTimeout(r, 30000))
await reader.cancel().catch(() => {})

// Reconstruct the final part table the way sync.tsx would (latest per part id).
const partById = new Map<string, any>()
let firstMsg = ""
for (const e of events) {
  const pl = e?.payload
  if (pl?.type === "message.part.updated") {
    const part = pl.properties.part
    partById.set(part.id, part)
  }
  if (pl?.type === "message.updated" && pl.properties?.info?.role === "assistant" && !firstMsg) firstMsg = pl.properties.info.id
}
const parts = [...partById.values()].sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0))
console.log("\ntotal GlobalEvents:", events.length)
console.log("ordered parts (id → type):")
for (const p of parts) {
  const extra =
    p.type === "tool" ? `  tool=${p.tool} state=${p.state?.status} out=${JSON.stringify(p.state?.output ?? p.state?.error ?? "").slice(0, 50)}` :
    p.type === "text" || p.type === "reasoning" ? `  text="${String(p.text ?? "").replace(/\n/g, " ").slice(0, 50)}"` : ""
  console.log(`  ${p.id}  [${p.type}]${extra}`)
}
const toolParts = parts.filter((p) => p.type === "tool")
console.log("\nTOOL PARTS:", toolParts.length, "| completed:", toolParts.filter((p) => p.state?.status === "completed").length)
process.exit(0)
