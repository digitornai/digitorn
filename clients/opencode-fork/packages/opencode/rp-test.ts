// Verify run_parallel expands into a header + native child ToolParts through
// the real adapter (digitornFetch).
import { digitornFetch, digitornConfig, daemonFetch } from "./src/cli/cmd/tui/context/digitorn"

const cfg = digitornConfig()
const f = digitornFetch(cfg)
const app = encodeURIComponent(cfg.app)
const created: any = await daemonFetch(cfg, `/api/apps/${app}/sessions`, {
  method: "POST",
  body: JSON.stringify({ title: "rp test", workdir: process.cwd() }),
})
const sid = created.session_id ?? created.id
console.log("session:", sid)

const partById = new Map<string, any>()
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
      if (line) try { const e = JSON.parse(line.slice(6)); if (e?.payload?.type === "message.part.updated") { const pt = e.payload.properties.part; partById.set(pt.id, pt) } } catch {}
    }
  }
})().catch(() => {})

await new Promise((r) => setTimeout(r, 400))
await f(`${cfg.url}/session/${sid}/prompt_async` as any, {
  method: "POST",
  body: JSON.stringify({ parts: [{ type: "text", text: "Explore ce projet en lançant PLUSIEURS filesystem.glob EN PARALLÈLE via run_parallel (au moins 4 patterns différents), puis résume brièvement." }] }),
})
console.log("prompt sent, 40s…")
await new Promise((r) => setTimeout(r, 40000))
await reader.cancel().catch(() => {})

const tools = [...partById.values()].filter((p) => p.type === "tool").sort((a, b) => (a.id < b.id ? -1 : 1))
console.log("\ntool parts:", tools.length)
for (const p of tools) {
  const inp = p.state?.input ? JSON.stringify(p.state.input).slice(0, 40) : ""
  const out = (p.state?.output ?? "").slice(0, 30).replace(/\n/g, " ")
  console.log(`  ${p.tool.padEnd(14)} ${p.state?.status.padEnd(10)} in=${inp} out="${out}"`)
}
process.exit(0)
