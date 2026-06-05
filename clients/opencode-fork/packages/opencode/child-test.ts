import { digitornFetch, digitornConfig, daemonFetch } from "./src/cli/cmd/tui/context/digitorn"
const cfg=digitornConfig(); const f=digitornFetch(cfg); const app=encodeURIComponent(cfg.app)
const c:any=await daemonFetch(cfg,`/api/apps/${app}/sessions`,{method:"POST",body:JSON.stringify({title:"child",workdir:process.cwd()})})
const sid=c.session_id??c.id; console.log("session:",sid)
const childTools=new Map<string,any>(); const tasks=new Map<string,any>()
const ev=await f(`${cfg.url}/event` as any); const rd=(ev.body as ReadableStream<Uint8Array>).getReader(); const dec=new TextDecoder(); let buf=""
;(async()=>{while(1){const{done,value}=await rd.read();if(done)break;buf+=dec.decode(value,{stream:true});let i;while((i=buf.indexOf("\n\n"))>=0){const l=buf.slice(0,i).split("\n").find(x=>x.startsWith("data: "));buf=buf.slice(i+2);if(l)try{const e=JSON.parse(l.slice(6));const pl=e?.payload;if(pl?.type==="message.part.updated"){const p=pl.properties.part;if(p.type==="tool"){if(String(p.sessionID).includes("::agent::"))childTools.set(p.id,p);else if(p.tool==="task")tasks.set(p.id,p)}}}catch{}}}})().catch(()=>{})
await new Promise(r=>setTimeout(r,400))
await f(`${cfg.url}/session/${sid}/prompt_async` as any,{method:"POST",body:JSON.stringify({parts:[{type:"text",text:"Délègue à 2 sous-agents explore en parallèle pour analyser src/ et package.json."}],agent:"build"})})
await new Promise(r=>setTimeout(r,28000))
console.log("parent task parts:",tasks.size)
console.log("CHILD-session tool parts:",childTools.size)
for(const p of [...childTools.values()].slice(0,8)) console.log(`  child ${p.tool.padEnd(6)} ${p.state?.status?.padEnd(9)} title="${p.state?.title||""}"`)
process.exit(0)
