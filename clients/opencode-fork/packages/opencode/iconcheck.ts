import { digitornFetch, digitornConfig } from "./src/cli/cmd/tui/context/digitorn"
const cfg = digitornConfig(); const f = digitornFetch(cfg)
const apps:any = await (await f(`${cfg.url}/digitorn/apps` as any)).json()
console.log("apps:", apps.length)
for (const a of apps.slice(0,6)) console.log(`  ${a.icon||"·"}  ${a.id.padEnd(16)} ${a.color.padEnd(9)} ${a.name}`)
