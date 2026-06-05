import { digitornFetch, digitornConfig, daemonFetch } from "./src/cli/cmd/tui/context/digitorn"
const cfg=digitornConfig(); const f=digitornFetch(cfg); const app=encodeURIComponent(cfg.app)
const c:any=await daemonFetch(cfg,`/api/apps/${app}/sessions`,{method:"POST",body:JSON.stringify({title:"task",workdir:process.cwd()})})
const sid=c.session_id??c.id; console.log("session:",sid)
const parts=new Map<string,any>()
const ev=await f(`${cfg.url}/event` as any); const rd=(ev.body as ReadableStream<Uint8Array>).getReader(); const dec=new TextDecoder(); let buf=""
;(async()=>{while(1){const{done,value}=await rd.read();if(done)break;buf+=dec.decode(value,{stream:true});let i;while((i=buf.indexOf("\n\n"))>=0){const l=buf.slice(0,i).split("\n").find(x=>x.startsWith("data: "));buf=buf.slice(i+2);if(l)try{const e=JSON.parse(l.slice(6));if(e?.payload?.type==="message.part.updated"){const p=e.payload.properties.part;if(p.type==="tool"&&p.tool==="task")parts.set(p.id,p)}}catch{}}}})().catch(()=>{})
await new Promise(r=>setTimeout(r,400))
await f(`${cfg.url}/session/${sid}/prompt_async` as any,{method:"POST",body:JSON.stringify({parts:[{type:"text",text:"Délègue à 2 sous-agents explore en parallèle pour analyser src/ et package.json."}],agent:"build"})})
await new Promise(r=>setTimeout(r,16000))
console.log("task parts:",parts.size)
for(const p of parts.values()) console.log(`  status=${p.state?.status} type=${p.state?.input?.subagent_type} sessionId=${p.state?.metadata?.sessionId?"SET":"-"} desc="${String(p.state?.input?.description||"").slice(0,40)}"`)
process.exit(0)
