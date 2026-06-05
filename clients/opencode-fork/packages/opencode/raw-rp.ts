import { digitornConfig, daemonFetch, connectDaemonEvents } from "./src/cli/cmd/tui/context/digitorn"
const cfg=digitornConfig(); const app=encodeURIComponent(cfg.app)
const c:any=await daemonFetch(cfg,`/api/apps/${app}/sessions`,{method:"POST",body:JSON.stringify({title:"rawrp",workdir:process.cwd()})})
const sid=c.session_id??c.id; console.log("session:",sid)
const t0=Date.now()
connectDaemonEvents(cfg,sid,(env)=>{
  if(env.type==="tool_call"||env.type==="tool_result"||env.type==="turn_ended"){
    const p:any=env.payload||{}
    console.log(`${String(Date.now()-t0).padStart(6)}ms ${env.type.padEnd(12)} call=${String(p.call_id||"").slice(-8)} name=${p.name||""} status=${p.status||""}`)
  }
},()=>{})
await new Promise(r=>setTimeout(r,500))
await daemonFetch(cfg,`/api/apps/${app}/sessions/${sid}/messages`,{method:"POST",body:JSON.stringify({content:"Liste les .ts avec glob et lis package.json en parallèle via run_parallel.",role:"user",mode:"build"})})
await new Promise(r=>setTimeout(r,35000))
process.exit(0)
