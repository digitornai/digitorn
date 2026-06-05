import { digitornConfig, daemonFetch } from "./src/cli/cmd/tui/context/digitorn"
const cfg = digitornConfig()
try {
  const r: any = await daemonFetch(cfg, `/api/apps`)
  console.log("type:", Array.isArray(r) ? "array" : typeof r, "keys:", r && typeof r === "object" ? Object.keys(r) : r)
  const list = Array.isArray(r) ? r : (r?.apps ?? [])
  console.log("count:", list.length, "first:", JSON.stringify(list[0] ?? null).slice(0, 120))
} catch (e: any) {
  console.log("THREW:", e?.message)
}
process.exit(0)
