import { digitornFetch, digitornConfig, daemonFetch } from "./src/cli/cmd/tui/context/digitorn"
const cfg = digitornConfig(); const f = digitornFetch(cfg); const app = encodeURIComponent(cfg.app)
const c:any = await daemonFetch(cfg, `/api/apps/${app}/sessions`, {method:"POST", body: JSON.stringify({title:"abort", workdir:"C:/tmp"})})
const sid = c.session_id ?? c.id
const r = await f(`${cfg.url}/session/${sid}/abort` as any, {method:"POST"})
console.log("POST /session/{id}/abort →", r.status)
process.exit(0)
