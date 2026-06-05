import { digitornFetch, digitornConfig, daemonFetch } from "./src/cli/cmd/tui/context/digitorn"
const cfg=digitornConfig(); const f=digitornFetch(cfg); const app=encodeURIComponent(cfg.app)
const c:any=await daemonFetch(cfg,`/api/apps/${app}/sessions`,{method:"POST",body:JSON.stringify({title:"meta",workdir:process.cwd()})})
const sid=c.session_id??c.id; console.log("session:",sid)
const parts=new Map<string,any>()
const ev=await f(`${cfg.url}/event` as any); const rd=(ev.body as ReadableStream<Uint8Array>).getReader(); const dec=new TextDecoder(); let buf=""
;(async()=>{while(1){const{done,value}=await rd.read();if(done)break;buf+=dec.decode(value,{stream:true});let i;while((i=buf.indexOf("\n\n"))>=0){const l=buf.slice(0,i).split("\n").find(x=>x.startsWith("data: "));buf=buf.slice(i+2);if(l)try{const e=JSON.parse(l.slice(6));if(e?.payload?.type==="message.part.updated"){const p=e.payload.properties.part;if(p.type==="tool")parts.set(p.id,p)}}catch{}}}})().catch(()=>{})
await new Promise(r=>setTimeout(r,400))
await f(`${cfg.url}/session/${sid}/prompt_async` as any,{method:"POST",body:JSON.stringify({parts:[{type:"text",text:"Lance glob '**/*.ts' et lis package.json en parallèle via run_parallel."}],agent:"build"})})
await new Promise(r=>setTimeout(r,22000))
for(const p of [...parts.values()].filter(p=>p.state?.status==="completed")) console.log(`  ${p.tool.padEnd(6)} status=${p.state.status} metadata=${JSON.stringify(p.state.metadata)}`)
process.exit(0)
