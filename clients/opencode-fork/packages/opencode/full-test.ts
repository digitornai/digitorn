import { digitornFetch, digitornConfig, daemonFetch } from "./src/cli/cmd/tui/context/digitorn"
const cfg=digitornConfig(); const f=digitornFetch(cfg); const app=encodeURIComponent(cfg.app)
const c:any=await daemonFetch(cfg,`/api/apps/${app}/sessions`,{method:"POST",body:JSON.stringify({title:"full",workdir:process.cwd()})})
const sid=c.session_id??c.id; console.log("session:",sid)
const all=new Map<string,any>()
const ev=await f(`${cfg.url}/event` as any); const rd=(ev.body as ReadableStream<Uint8Array>).getReader(); const dec=new TextDecoder(); let buf=""
;(async()=>{while(1){const{done,value}=await rd.read();if(done)break;buf+=dec.decode(value,{stream:true});let i;while((i=buf.indexOf("\n\n"))>=0){const l=buf.slice(0,i).split("\n").find(x=>x.startsWith("data: "));buf=buf.slice(i+2);if(l)try{const e=JSON.parse(l.slice(6));const pl=e?.payload;if(pl?.type==="message.part.updated"&&pl.properties.part.type==="tool")all.set(pl.properties.part.id,pl.properties.part)}catch{}}}})().catch(()=>{})
await new Promise(r=>setTimeout(r,400))
await f(`${cfg.url}/session/${sid}/prompt_async` as any,{method:"POST",body:JSON.stringify({parts:[{type:"text",text:"Délègue à 2 sous-agents explore (un pour src/, un pour package.json) et résume."}],agent:"build"})})
await new Promise(r=>setTimeout(r,26000))
for(const p of all.values()){const child=String(p.sessionID).includes("::agent::");console.log(`${child?"  CHILD":"PARENT"} ${p.tool.padEnd(12)} ${p.state?.status?.padEnd(9)} ${p.tool==="task"?"desc="+JSON.stringify(p.state?.input?.description||"").slice(0,30):"title="+(p.state?.title||"")}`)}
process.exit(0)
