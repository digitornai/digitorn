import { createSignal, onCleanup, onMount, Show } from "solid-js"
import { RGBA } from "@opentui/core"
import { useSDK } from "../context/sdk"
import { useTheme } from "@tui/context/theme"

// The ACTIVE digitorn app, shown left-aligned under the home input. Reflects the
// current app and updates live when it changes — the adapter pushes a digitorn.app
// event on every switch (via the picker), same push pattern as conn/context.
type Active = { id: string; name: string; icon: string; color: string }

const hexRGBA = (h: string): RGBA | undefined => {
  const m = /^#?([0-9a-fA-F]{6})$/.exec(h ?? "")
  if (!m) return undefined
  const n = parseInt(m[1], 16)
  return RGBA.fromInts((n >> 16) & 255, (n >> 8) & 255, n & 255, 255)
}

export function HomeApps() {
  const sdk = useSDK()
  const { theme } = useTheme()
  const [app, setApp] = createSignal<Active>()

  onMount(() => {
    // Initial : the app currently marked current.
    sdk
      .fetch(`${sdk.url}/digitorn/apps`)
      .then((r) => r.json())
      .then((list: any[]) => {
        const cur = (list ?? []).find((a) => a.current)
        if (cur) setApp({ id: cur.id, name: cur.name, icon: cur.icon ?? "", color: cur.color ?? "" })
      })
      .catch(() => {})
    // Live : the adapter pushes digitorn.app whenever the active app changes.
    const unsub = sdk.event.on("event", (ev: any) => {
      const p = ev?.payload
      if (p?.type !== "digitorn.app") return
      setApp({ id: p.properties.id, name: p.properties.name, icon: p.properties.icon ?? "", color: p.properties.color ?? "" })
    })
    onCleanup(() => unsub())
  })

  const iconColor = () => hexRGBA(app()?.color ?? "") ?? theme.textMuted

  return (
    <Show when={app()}>
      <box flexDirection="row" gap={1} justifyContent="flex-start">
        <text fg={iconColor()}>{app()!.icon || "▪"}</text>
        <text fg={theme.text}>{app()!.name}</text>
      </box>
    </Show>
  )
}
