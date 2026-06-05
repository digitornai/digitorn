import { digitornFetch, digitornConfig, daemonFetch } from "./src/cli/cmd/tui/context/digitorn"
const cfg = digitornConfig(); const f = digitornFetch(cfg); const app = encodeURIComponent(cfg.app)
const c: any = await daemonFetch(cfg, `/api/apps/${app}/sessions`, { method:"POST", body: JSON.stringify({ title:"order", workdir: process.cwd() }) })
const sid = c.session_id ?? c.id; console.log("session:", sid)
const partById = new Map<string, any>()
const evRes = await f(`${cfg.url}/event` as any); const reader = (evRes.body as ReadableStream<Uint8Array>).getReader(); const dec = new TextDecoder(); let buf=""
;(async()=>{while(true){const{done,value}=await reader.read();if(done)break;buf+=dec.decode(value,{stream:true});let i:number;while((i=buf.indexOf("\n\n"))>=0){const line=buf.slice(0,i).split("\n").find(l=>l.startsWith("data: "));buf=buf.slice(i+2);if(line)try{const e=JSON.parse(line.slice(6));if(e?.payload?.type==="message.part.updated"){const pt=e.payload.properties.part;partById.set(pt.id,pt)}}catch{}}}})().catch(()=>{})
await new Promise(r=>setTimeout(r,400))
await f(`${cfg.url}/session/${sid}/prompt_async` as any,{method:"POST",body:JSON.stringify({parts:[{type:"text",text:"Réfléchis, puis lance run_parallel avec 3 filesystem.glob, puis dis une phrase."}]})})
console.log("sent, 30s…"); await new Promise(r=>setTimeout(r,30000))
const parts=[...partById.values()].sort((a,b)=>a.id<b.id?-1:1)
console.log("ordered parts (sorted by id = daemon seq):")
for(const p of parts){const tag=p.type==="tool"?`tool:${p.tool}`:p.type;console.log(`  ${p.id.replace(sid,"S")}  [${tag}]`)}
process.exit(0)
