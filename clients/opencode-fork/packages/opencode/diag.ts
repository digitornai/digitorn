import { digitornFetch, digitornConfig } from "./src/cli/cmd/tui/context/digitorn"
const cfg = digitornConfig(); const f = digitornFetch(cfg)
async function j(p:string,init?:any){const r=await f(`${cfg.url}${p}` as any,init);let b:any;try{b=await r.json()}catch{b="<non-json>"}return {status:r.status,b}}
const apps = await j("/digitorn/apps"); console.log("GET /digitorn/apps:", apps.status, Array.isArray(apps.b)?`${apps.b.length} apps`:apps.b)
const list = await j("/session"); console.log("GET /session:", list.status, Array.isArray(list.b)?`${list.b.length} sessions`:JSON.stringify(list.b).slice(0,100))
const created = await j("/session",{method:"POST",body:JSON.stringify({title:"diag"})}); console.log("POST /session:", created.status, JSON.stringify(created.b).slice(0,150))
process.exit(0)
