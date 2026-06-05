import { digitornFetch, digitornConfig, daemonFetch } from "./src/cli/cmd/tui/context/digitorn"
const cfg=digitornConfig(); const f=digitornFetch(cfg); const app=encodeURIComponent(cfg.app)
const c:any=await daemonFetch(cfg,`/api/apps/${app}/sessions`,{method:"POST",body:JSON.stringify({title:"inspect",workdir:process.cwd()})})
const sid=c.session_id??c.id; console.log("session:",sid)
const parts=new Map<string,any>(); const seenStatus=new Map<string,Set<string>>()
const ev=await f(`${cfg.url}/event` as any); const rd=(ev.body as ReadableStream<Uint8Array>).getReader(); const dec=new TextDecoder(); let buf=""
;(async()=>{while(1){const{done,value}=await rd.read();if(done)break;buf+=dec.decode(value,{stream:true});let i;while((i=buf.indexOf("\n\n"))>=0){const l=buf.slice(0,i).split("\n").find(x=>x.startsWith("data: "));buf=buf.slice(i+2);if(l)try{const e=JSON.parse(l.slice(6));if(e?.payload?.type==="message.part.updated"){const p=e.payload.properties.part;if(p.type==="tool"){parts.set(p.id,p);if(!seenStatus.has(p.id))seenStatus.set(p.id,new Set());seenStatus.get(p.id)!.add(p.state?.status)}}}catch{}}}})().catch(()=>{})
await new Promise(r=>setTimeout(r,400))
await f(`${cfg.url}/session/${sid}/prompt_async` as any,{method:"POST",body:JSON.stringify({parts:[{type:"text",text:"Analyse brièvement ce projet (lis package.json, glob les .ts)."}],agent:"build"})})
await new Promise(r=>setTimeout(r,24000))
const MAPPED=new Set(["read","write","edit","glob","grep","bash","websearch","webfetch"])
for(const p of parts.values()){
  const inp=JSON.stringify(p.state?.input||{})
  const out=(p.state?.output||"").replace(/\n/g," ").slice(0,55)
  console.log(`[${MAPPED.has(p.tool)?"native":"GENERIC"}] ${p.tool.padEnd(8)} status(${[...(seenStatus.get(p.id)||[])].join("→")}) input=${inp.slice(0,45)} out="${out}"`)
}
process.exit(0)
