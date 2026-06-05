// Capture the RAW run_parallel tool_call + tool_result + tool_progress payloads
// to design how to expand its children into individual ToolParts.
import { digitornConfig, daemonFetch, connectDaemonEvents } from "./src/cli/cmd/tui/context/digitorn"

const cfg = digitornConfig()
const app = encodeURIComponent(cfg.app)
const created: any = await daemonFetch(cfg, `/api/apps/${app}/sessions`, {
  method: "POST",
  body: JSON.stringify({ title: "rp capture", workdir: process.cwd() }),
})
const sid = created.session_id ?? created.id
console.log("session:", sid)

const hits: any[] = []
connectDaemonEvents(cfg, sid, (env) => {
  if ((env.type === "tool_call" || env.type === "tool_result" || env.type === "tool_progress") &&
      JSON.stringify(env.payload).includes("run_parallel")) {
    hits.push({ type: env.type, seq: env.seq, corr: (env as any).correlation_id, payload: env.payload })
  }
}, () => {})

await new Promise((r) => setTimeout(r, 500))
await daemonFetch(cfg, `/api/apps/${app}/sessions/${sid}/messages`, {
  method: "POST",
  body: JSON.stringify({ content: "Analyse ce projet : explore la structure en lançant PLUSIEURS recherches de fichiers EN PARALLÈLE (utilise run_parallel avec plusieurs filesystem.glob). Puis résume.", role: "user", mode: "" }),
})
console.log("prompt sent, 40s…")
await new Promise((r) => setTimeout(r, 40000))

console.log("\nrun_parallel-related events:", hits.length)
for (const h of hits) {
  console.log(`\n[${h.type}] seq=${h.seq}`)
  console.log(JSON.stringify(h.payload, null, 2).slice(0, 1400))
}
process.exit(0)
