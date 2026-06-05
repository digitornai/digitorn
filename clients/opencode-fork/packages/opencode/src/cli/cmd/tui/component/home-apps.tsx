import { createEffect, createResource, createSignal, For, Show } from "solid-js"
import { RGBA } from "@opentui/core"
import { useSDK } from "../context/sdk"
import { useTheme } from "@tui/context/theme"

// Quick-launch row under the home input: the first digitorn apps with their icon
// + brand color. Click one to make it the active app (quiet switch — the next
// prompt creates a session in it), then just type and send.
type App = { id: string; name: string; description: string; category: string; icon: string; color: string; current: boolean }

const hexRGBA = (h: string, alpha = 255): RGBA | undefined => {
  const m = /^#?([0-9a-fA-F]{6})$/.exec(h ?? "")
  if (!m) return undefined
  const n = parseInt(m[1], 16)
  return RGBA.fromInts((n >> 16) & 255, (n >> 8) & 255, n & 255, alpha)
}

export function HomeApps() {
  const sdk = useSDK()
  const { theme } = useTheme()

  const [apps] = createResource(async () => {
    try {
      return (await (await sdk.fetch(`${sdk.url}/digitorn/apps`)).json()) as App[]
    } catch {
      return [] as App[]
    }
  })

  const [selected, setSelected] = createSignal<string>()
  createEffect(() => {
    const cur = (apps() ?? []).find((a) => a.current)
    if (cur && !selected()) setSelected(cur.id)
  })

  const four = () => (apps() ?? []).slice(0, 4)
  const pick = async (id: string) => {
    setSelected(id)
    try {
      await sdk.fetch(`${sdk.url}/digitorn/app`, { method: "POST", body: JSON.stringify({ app_id: id, quiet: true }) })
    } catch {}
  }

  return (
    <Show when={four().length}>
      <box flexDirection="row" gap={2} paddingTop={1} justifyContent="center">
        <For each={four()}>
          {(a) => {
            const on = () => selected() === a.id
            const iconColor = () => hexRGBA(a.color) ?? theme.textMuted
            return (
              <box
                flexDirection="row"
                gap={1}
                paddingLeft={1}
                paddingRight={1}
                backgroundColor={on() ? theme.backgroundElement : RGBA.fromInts(0, 0, 0, 0)}
                onMouseUp={() => pick(a.id)}
              >
                <text fg={iconColor()}>{a.icon || "▪"}</text>
                <text fg={on() ? theme.text : theme.textMuted}>{a.name}</text>
              </box>
            )
          }}
        </For>
      </box>
    </Show>
  )
}
