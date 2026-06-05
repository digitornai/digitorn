import { digitornConfig, daemonFetch, connectDaemonEvents } from "./src/cli/cmd/tui/context/digitorn"
const cfg=digitornConfig(); const app=encodeURIComponent(cfg.app)
const c:any=await daemonFetch(cfg,`/api/apps/${app}/sessions`,{method:"POST",body:JSON.stringify({title:"agcap",workdir:process.cwd()})})
const sid=c.session_id??c.id; console.log("session:",sid)
const seen:any[]=[]
connectDaemonEvents(cfg,sid,(env)=>{
  const t=env.type
  if(t==="agent_spawn"||t==="agent_result"||((t==="tool_call"||t==="tool_result")&&JSON.stringify(env.payload).match(/agent|spawn|explore|subagent/i))){
    seen.push({t,payload:env.payload})
  }
},()=>{})
await new Promise(r=>setTimeout(r,500))
await daemonFetch(cfg,`/api/apps/${app}/sessions/${sid}/messages`,{method:"POST",body:JSON.stringify({content:"Délègue à 2 sous-agents 'explore' en parallèle pour analyser src/ et package.json, puis résume.",role:"user",mode:"build"})})
await new Promise(r=>setTimeout(r,40000))
for(const s of seen.slice(0,10)){console.log(`\n[${s.t}]`);console.log(JSON.stringify(s.payload,null,1).slice(0,700))}
process.exit(0)
