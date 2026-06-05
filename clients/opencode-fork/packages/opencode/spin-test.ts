import { digitornFetch, digitornConfig, daemonFetch } from "./src/cli/cmd/tui/context/digitorn"
const cfg=digitornConfig(); const f=digitornFetch(cfg); const app=encodeURIComponent(cfg.app)
const c:any=await daemonFetch(cfg,`/api/apps/${app}/sessions`,{method:"POST",body:JSON.stringify({title:"spin",workdir:process.cwd()})})
const sid=c.session_id??c.id; console.log("session:",sid)
const t0=Date.now()
const log:any[]=[]
const ev=await f(`${cfg.url}/event` as any); const rd=(ev.body as ReadableStream<Uint8Array>).getReader(); const dec=new TextDecoder(); let buf=""
;(async()=>{while(1){const{done,value}=await rd.read();if(done)break;buf+=dec.decode(value,{stream:true});let i;while((i=buf.indexOf("\n\n"))>=0){const l=buf.slice(0,i).split("\n").find(x=>x.startsWith("data: "));buf=buf.slice(i+2);if(l)try{const e=JSON.parse(l.slice(6));if(e?.payload?.type==="message.part.updated"){const p=e.payload.properties.part;if(p.type==="tool")log.push({ms:Date.now()-t0,id:p.id.slice(-10),tool:p.tool,status:p.state?.status})}}catch{}}}})().catch(()=>{})
await new Promise(r=>setTimeout(r,400))
await f(`${cfg.url}/session/${sid}/prompt_async` as any,{method:"POST",body:JSON.stringify({parts:[{type:"text",text:"Liste les .ts avec glob et lis package.json en parallèle via run_parallel."}],agent:"build"})})
await new Promise(r=>setTimeout(r,25000))
// per-tool: running start → completed end gap
const byId=new Map<string,{tool:string,running?:number,done?:number}>()
for(const e of log){const r=byId.get(e.id)||{tool:e.tool};if(e.status==="running"&&r.running==null)r.running=e.ms;if(e.status==="completed"||e.status==="error")r.done=e.ms;byId.set(e.id,r)}
console.log("per-tool running→completed (ms):")
for(const [id,r] of byId) console.log(`  ${r.tool.padEnd(7)} running@${r.running??"-"} done@${r.done??"-"} gap=${r.running!=null&&r.done!=null?(r.done-r.running)+"ms":"?"}`)
process.exit(0)
