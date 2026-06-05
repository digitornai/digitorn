import { digitornFetch, digitornConfig } from "./src/cli/cmd/tui/context/digitorn"
const cfg = digitornConfig(); const f = digitornFetch(cfg)
const agents:any = await (await f(`${cfg.url}/agent` as any)).json()
console.log("agents (modes):", agents.map((a:any)=>`${a.name}${a.mode==='primary'?'':' ['+a.mode+']'}`).join(", "))
for (const a of agents) console.log(`  ${a.name.padEnd(8)} ${a.description||""}`)
