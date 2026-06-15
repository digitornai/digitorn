// Test end-to-end pieces-probe : open SSE first, send message, wait for turn_complete.
import { digitornConfig, daemonFetch } from "./src/cli/cmd/tui/context/digitorn"

const cfg = digitornConfig()
const app = process.env.DIGITORN_APP ?? "json-probe"

// 1. Create session
const created: any = await daemonFetch(cfg, `/api/apps/${app}/sessions`, {
  method: "POST",
  body: JSON.stringify({ title: "pieces e2e test" }),
})
const sid = created.session_id ?? created.id
if (!sid) { console.error("session create failed:", JSON.stringify(created)); process.exit(1) }
console.log("session:", sid)

// 2. Open SSE stream BEFORE sending message
const evRes = await fetch(`${cfg.url}/api/apps/${app}/sessions/${sid}/events`, {
  headers: { authorization: `Bearer ${cfg.token}`, accept: "text/event-stream" },
})
console.log("SSE status:", evRes.status)
const reader = (evRes.body as ReadableStream<Uint8Array>).getReader()
const dec = new TextDecoder()
let buf = ""
const events: any[] = []
let done = false

const collect = (async () => {
  while (!done) {
    const { done: streamDone, value } = await reader.read()
    if (streamDone) break
    buf += dec.decode(value, { stream: true })
    let i: number
    while ((i = buf.indexOf("\n\n")) >= 0) {
      const chunk = buf.slice(0, i)
      buf = buf.slice(i + 2)
      const line = chunk.split("\n").find((l) => l.startsWith("data: "))
      if (!line) continue
      try {
        const ev = JSON.parse(line.slice(6))
        events.push(ev)
        const t = ev?.type ?? ev?.event ?? "?"
        process.stdout.write(` [${t}]`)
      } catch {}
    }
  }
})()

// 3. Small delay so stream is fully open
await new Promise((r) => setTimeout(r, 300))

// 4. Send message
const msg: any = await daemonFetch(cfg, `/api/apps/${app}/sessions/${sid}/messages`, {
  method: "POST",
  body: JSON.stringify({
    content: 'Use the tool ap_json__convert_text_to_json to parse this text: {"name":"Paul","age":30}. Report verbatim what the tool returned.',
  }),
})
console.log("\nmessage sent:", msg?.role ?? JSON.stringify(msg).slice(0, 80))

// 5. Wait up to 60s for turn_complete
const deadline = Date.now() + 60_000
while (Date.now() < deadline) {
  const last = events[events.length - 1]
  const t = last?.type ?? last?.event ?? ""
  if (t === "turn_complete" || t === "turn_end" || t === "message_complete" || t === "error") {
    console.log("\nturn ended with:", t)
    break
  }
  await new Promise((r) => setTimeout(r, 500))
}
done = true
await reader.cancel().catch(() => {})
await collect.catch(() => {})

// 6. Report
console.log("\n\ntotal events:", events.length)
const types = [...new Set(events.map((e) => e?.type ?? e?.event))]
console.log("types:", JSON.stringify(types))

const toolEvents = events.filter((e) => {
  const t = (e?.type ?? "").toLowerCase()
  return t.includes("tool") || t.includes("call")
})
console.log("tool-related events:", toolEvents.length)
if (toolEvents.length > 0) {
  for (const e of toolEvents.slice(0, 3)) {
    console.log(" •", JSON.stringify(e).slice(0, 400))
  }
}

const errorEvents = events.filter((e) => (e?.type ?? "").toLowerCase().includes("error"))
if (errorEvents.length > 0) {
  console.log("ERRORS:")
  for (const e of errorEvents) console.log(" •", JSON.stringify(e).slice(0, 400))
}

// Final history
const hist: any = await daemonFetch(cfg, `/api/apps/${app}/sessions/${sid}/history`)
const msgs = Array.isArray(hist) ? hist : (hist?.messages ?? hist?.history ?? [])
console.log("\nhistory messages:", msgs.length)
const lastAssistant = [...msgs].reverse().find((m: any) => m.role === "assistant")
if (lastAssistant) {
  console.log("assistant:", JSON.stringify(lastAssistant.content ?? lastAssistant.text ?? "").slice(0, 1000))
}

process.exit(0)
