// Verify reasoning parts get time.end (thinking stops) + segment per round.
import { digitornFetch, digitornConfig, daemonFetch } from "./src/cli/cmd/tui/context/digitorn"
const cfg = digitornConfig()
const f = digitornFetch(cfg)
const app = encodeURIComponent(cfg.app)
const created: any = await daemonFetch(cfg, `/api/apps/${app}/sessions`, {
  method: "POST",
  body: JSON.stringify({ title: "reason test", workdir: process.cwd() }),
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
  body: JSON.stringify({ parts: [{ type: "text", text: "Liste les fichiers .ts du dossier courant avec glob, réfléchis à ce que tu vois, puis résume." }] }),
})
console.log("prompt sent, 28s…")
await new Promise((r) => setTimeout(r, 28000))
const reasoning = [...partById.values()].filter((p) => p.type === "reasoning").sort((a, b) => (a.id < b.id ? -1 : 1))
console.log("\nreasoning parts:", reasoning.length)
for (const p of reasoning) console.log(`  ${p.id.slice(-6)}  end=${p.time?.end !== undefined ? "SET ✓" : "MISSING ✗"}  len=${(p.text ?? "").length}`)
const allDone = reasoning.every((p) => p.time?.end !== undefined)
console.log(allDone && reasoning.length ? "\n✓ all thinking blocks closed (time.end set)" : "\n✗ some thinking never stops")
process.exit(0)
