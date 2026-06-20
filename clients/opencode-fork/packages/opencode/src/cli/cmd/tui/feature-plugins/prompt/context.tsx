import type { TuiPlugin, TuiPluginApi } from "@opencode-ai/plugin/tui"
import type { InternalTuiPlugin } from "../../plugin/internal"
import { createSignal, createEffect, onCleanup, onMount } from "solid-js"
import { TextAttributes } from "@opentui/core"
import { useSDK } from "@tui/context/sdk"

const id = "internal:prompt-context"

// Default window shown before the daemon's first recount lands, so the gauge
// reads as context occupancy from the first frame (matches the old CLI's
// defaultCtxWindow = 8192).
const DEFAULT_WINDOW = 8192

// humanizeTokens — ported verbatim from the old Go CLI (widget_messages.go) so
// the gauge reads identically : "0", "842", "8.2k", "16k", "1.3M".
function humanizeTokens(n: number): string {
  if (n <= 0) return "0"
  if (n < 1000) return String(n)
  const trim = (v: number) => {
    const s = v.toFixed(1)
    return s.endsWith(".0") ? s.slice(0, -2) : s
  }
  if (n < 1_000_000) return trim(n / 1000) + "k"
  return trim(n / 1_000_000) + "M"
}

// The old CLI's input footer right-counter : "ctx used/window", climbing as the
// conversation grows and dropping on compaction. Lives on the SAME line as the
// mode/model (prompt footer), at the right edge. Same PUSHED source as the
// sidebar Context panel (digitorn.context), one snapshot on mount then live.
function View(props: { api: TuiPluginApi; session_id: string }) {
  const theme = () => props.api.theme.current
  const sdk = useSDK()
  const [ctx, setCtx] = createSignal<{ tokens: number; window: number }>({ tokens: 0, window: 0 })

  createEffect(() => {
    const sid = props.session_id
    sdk
      .fetch(`${sdk.url}/digitorn/context?session=${encodeURIComponent(sid)}`)
      .then((r) => r.json())
      .then((j: any) => setCtx({ tokens: j?.tokens ?? 0, window: j?.window ?? 0 }))
      .catch(() => {})
    const unsub = sdk.event.on("event", (ev: any) => {
      const p = ev?.payload
      if (p?.type !== "digitorn.context" || p.properties?.session !== sid) return
      setCtx({ tokens: p.properties.tokens ?? 0, window: p.properties.window ?? 0 })
    })
    onCleanup(() => unsub())
  })

  const used = () => ctx().tokens
  const window = () => (ctx().window > 0 ? ctx().window : DEFAULT_WINDOW)
  const color = () => {
    const u = used()
    const w = window()
    if (u * 100 >= w * 90) return theme().error
    if (u * 100 >= w * 75) return theme().warning
    return theme().textMuted
  }

  return (
    <text attributes={TextAttributes.DIM} fg={color()}>
      {`ctx ${humanizeTokens(used())}/${humanizeTokens(window())}`}
    </text>
  )
}

const tui: TuiPlugin = async (api) => {
  api.slots.register({
    order: 100,
    slots: {
      // Right edge of the prompt footer — SAME line as the mode/model. Digitorn
      // only (the gauge reads OUR daemon's context_tokens); stock opencode keeps
      // its empty right slot.
      session_prompt_right(_ctx, props) {
        if (!process.env.DIGITORN_URL) return null
        return <View api={api} session_id={props.session_id} />
      },
    },
  })
}

const plugin: InternalTuiPlugin = {
  id,
  tui,
}

export default plugin
