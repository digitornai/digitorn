import { digitornFetch, digitornConfig, daemonFetch } from "./src/cli/cmd/tui/context/digitorn"
const cfg = digitornConfig(); const f = digitornFetch(cfg); const app = encodeURIComponent(cfg.app)
const c: any = await daemonFetch(cfg, `/api/apps/${app}/sessions`, { method:"POST", body: JSON.stringify({ title:"rpg", workdir: process.cwd() }) })
const sid = c.session_id ?? c.id; console.log("session:", sid)
const partById = new Map<string, any>()
const evRes = await f(`${cfg.url}/event` as any); const reader = (evRes.body as ReadableStream<Uint8Array>).getReader(); const dec = new TextDecoder(); let buf=""
;(async()=>{while(true){const{done,value}=await reader.read();if(done)break;buf+=dec.decode(value,{stream:true});let i:number;while((i=buf.indexOf("\n\n"))>=0){const line=buf.slice(0,i).split("\n").find(l=>l.startsWith("data: "));buf=buf.slice(i+2);if(line)try{const e=JSON.parse(line.slice(6));if(e?.payload?.type==="message.part.updated"){const pt=e.payload.properties.part;partById.set(pt.id,pt)}}catch{}}}})().catch(()=>{})
await new Promise(r=>setTimeout(r,400))
await f(`${cfg.url}/session/${sid}/prompt_async` as any,{method:"POST",body:JSON.stringify({parts:[{type:"text",text:"Lance run_parallel avec 3 filesystem.glob (patterns différents), puis résume."}]})})
console.log("sent, 30s…"); await new Promise(r=>setTimeout(r,30000))
const rp=[...partById.values()].filter(p=>p.tool==="run_parallel")
console.log("run_parallel parts:",rp.length)
for(const p of rp){const ch=p.state?.metadata?.children??[];console.log(`  status=${p.state?.status} children=${ch.length}`);for(const k of ch)console.log(`    - ${k.tool} ${JSON.stringify(k.input)} [${k.status}] out=${(k.output||"").length}c`)}
const others=[...partById.values()].filter(p=>p.type==="tool"&&p.tool!=="run_parallel")
console.log("other (non-grouped) tool parts:",others.length)
process.exit(0)
