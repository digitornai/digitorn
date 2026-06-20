import type { TuiPlugin, TuiPluginApi } from "@opencode-ai/plugin/tui"
import type { InternalTuiPlugin } from "../../plugin/internal"
import { createSignal, createEffect, onCleanup, onMount } from "solid-js"
import { useSDK } from "@tui/context/sdk"

const id = "internal:sidebar-context"

type Ctx = { tokens: number; window: number; percent: number }

function View(props: { api: TuiPluginApi; session_id: string }) {
  const theme = () => props.api.theme.current
  const sdk = useSDK()
  // Occupancy is the daemon's authoritative count (context_tokens total/window)
  // — the SAME source the Go CLI footer gauge reads. PUSHED, not polled: the
  // adapter broadcasts digitorn.context on every recount through the chat event
  // stream. One snapshot on mount, then live. (No $ spent — the daemon does not
  // track cost yet.)
  const [ctx, setCtx] = createSignal<Ctx>({ tokens: 0, window: 0, percent: 0 })
  createEffect(() => {
    const sid = props.session_id
    sdk
      .fetch(`${sdk.url}/digitorn/context?session=${encodeURIComponent(sid)}`)
      .then((r) => r.json())
      .then((j: any) => setCtx({ tokens: j?.tokens ?? 0, window: j?.window ?? 0, percent: j?.percent ?? 0 }))
      .catch(() => {})
    const unsub = sdk.event.on("event", (ev: any) => {
      const p = ev?.payload
      if (p?.type !== "digitorn.context" || p.properties?.session !== sid) return
      setCtx({
        tokens: p.properties.tokens ?? 0,
        window: p.properties.window ?? 0,
        percent: p.properties.percent ?? 0,
      })
    })
    onCleanup(() => unsub())
  })

  return (
    <box>
      <text fg={theme().text}>
        <b>Context</b>
      </text>
      <text fg={theme().textMuted}>{ctx().tokens.toLocaleString()} tokens</text>
      <text fg={theme().textMuted}>{ctx().percent}% used</text>
    </box>
  )
}

const tui: TuiPlugin = async (api) => {
  api.slots.register({
    order: 100,
    slots: {
      sidebar_content(_ctx, props) {
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
