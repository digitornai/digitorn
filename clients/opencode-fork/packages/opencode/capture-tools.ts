// Ground-truth capture : run a tool-forcing prompt on claude-code and log every
// RAW daemon envelope (type + payload) so the adapter's tool_call/tool_result →
// ToolPart translation is built from real shapes, not guesses.
import { digitornConfig, daemonFetch, connectDaemonEvents, type DaemonEnvelope } from "./src/cli/cmd/tui/context/digitorn"

const cfg = digitornConfig()
const app = encodeURIComponent(cfg.app)

const created: any = await daemonFetch(cfg, `/api/apps/${app}/sessions`, {
  method: "POST",
  body: JSON.stringify({ title: "tool capture", workdir: process.cwd() }),
})
const sid = created.session_id ?? created.id
console.log("session:", sid, "app:", cfg.app)

const seen: DaemonEnvelope[] = []
const conn = connectDaemonEvents(cfg, sid, (env) => {
  seen.push(env)
}, () => console.log("socket connected"))

await new Promise((r) => setTimeout(r, 600))
conn.join(sid)
await daemonFetch(cfg, `/api/apps/${app}/sessions/${sid}/messages`, {
  method: "POST",
  body: JSON.stringify({ content: "Liste les fichiers du dossier courant en utilisant l'outil approprié, puis dis combien il y en a.", role: "user", mode: "" }),
})
console.log("prompt sent, capturing 30s…")
await new Promise((r) => setTimeout(r, 30000))
conn.close()

console.log("\n=== event type histogram ===")
const hist: Record<string, number> = {}
for (const e of seen) hist[e.type] = (hist[e.type] ?? 0) + 1
console.log(JSON.stringify(hist, null, 2))

console.log("\n=== tool_call / tool_result / tool_progress raw payloads ===")
for (const e of seen) {
  if (e.type === "tool_call" || e.type === "tool_result" || e.type === "tool_progress") {
    console.log(`\n[${e.type}] seq=${e.seq} corr=${(e as any).correlation_id ?? ""}`)
    console.log(JSON.stringify(e.payload, null, 2).slice(0, 900))
  }
}
process.exit(0)
