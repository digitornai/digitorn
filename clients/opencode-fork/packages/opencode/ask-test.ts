// Trigger ask_user and verify question.asked → reply → turn resumes.
import { digitornFetch, digitornConfig, daemonFetch } from "./src/cli/cmd/tui/context/digitorn"

const cfg = digitornConfig()
const f = digitornFetch(cfg)
const app = encodeURIComponent(cfg.app)

const created: any = await daemonFetch(cfg, `/api/apps/${app}/sessions`, {
  method: "POST",
  body: JSON.stringify({ title: "ask test", workdir: process.cwd() }),
})
const sid = created.session_id ?? created.id
console.log("session:", sid)

let asked: any = null
let answered = false
const seen: string[] = []
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
      if (!line) continue
      try {
        const e = JSON.parse(line.slice(6))
        const t = e?.payload?.type
        if (t) seen.push(t)
        if (t === "question.asked" && !asked) {
          asked = e.payload.properties
          console.log("→ question.asked:", JSON.stringify(asked.questions?.[0]))
          // Reply through the real adapter route.
          answered = true
          await f(`${cfg.url}/question/${asked.id}/reply` as any, {
            method: "POST",
            body: JSON.stringify({ answers: [["Paul"]] }),
          })
          console.log("→ replied 'Paul' via /question/{id}/reply")
        }
        if (t === "question.replied") console.log("→ question.replied (dismissed)")
      } catch {}
    }
  }
})().catch(() => {})

await new Promise((r) => setTimeout(r, 400))
await f(`${cfg.url}/session/${sid}/prompt_async` as any, {
  method: "POST",
  body: JSON.stringify({ parts: [{ type: "text", text: "Utilise impérativement l'outil ask_user pour me demander mon prénom (question ouverte, sans choix), PUIS salue-moi par ce prénom." }] }),
})
console.log("prompt sent, 45s…")
await new Promise((r) => setTimeout(r, 45000))
await reader.cancel().catch(() => {})

console.log("\nquestion.asked seen:", seen.filter((t) => t === "question.asked").length)
console.log("answered:", answered, "| question.replied:", seen.filter((t) => t === "question.replied").length)
const hist: any = await daemonFetch(cfg, `/api/apps/${app}/sessions/${sid}/history?limit=20`)
const last = (hist.messages ?? []).filter((m: any) => m.role === "assistant").pop()
console.log("final assistant reply:", JSON.stringify((last?.content ?? "").slice(0, 120)))
process.exit(0)
