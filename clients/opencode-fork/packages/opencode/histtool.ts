import { digitornFetch, digitornConfig } from "./src/cli/cmd/tui/context/digitorn"
const cfg = digitornConfig()
const f = digitornFetch(cfg)
const sid = "f2012565-55db-4e0e-bffe-d94eab6dba80"
const res = await f(`${cfg.url}/session/${sid}/message` as any)
const msgs: any[] = await res.json()
console.log("messages:", msgs.length)
for (const m of msgs) {
  const parts = m.parts.map((p: any) => (p.type === "tool" ? `tool:${p.tool}(${p.state?.status})` : p.type)).join(", ")
  console.log(` ${m.info.role}: [${parts}]`)
}
const toolParts = msgs.flatMap((m) => m.parts).filter((p: any) => p.type === "tool")
console.log("TOOL PARTS in history:", toolParts.length, toolParts.map((p: any) => `${p.tool}/${p.state?.status}`).join(","))
process.exit(0)
