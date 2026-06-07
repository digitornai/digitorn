import { TextAttributes } from "@opentui/core"
import { createResource, For, Show, type JSX } from "solid-js"
import { useTheme } from "../context/theme"
import { useDialog } from "@tui/ui/dialog"
import { useSDK } from "@tui/context/sdk"
import { useRoute } from "@tui/context/route"

type Status = {
  app: { id: string; name: string; version: string; color?: string }
  daemon: { state: string; url: string }
  context: { tokens: number; window: number; percent: number }
  model: string
  modules: string[]
}

export type DialogStatusProps = {}

export function DialogStatus() {
  const { theme } = useTheme()
  const dialog = useDialog()
  const sdk = useSDK()
  const route = useRoute()
  const sid = () => (route.data.type === "session" ? route.data.sessionID : "")

  const [status] = createResource(async () => {
    try {
      const res = await sdk.fetch(`${sdk.url}/digitorn/status?session=${encodeURIComponent(sid())}`)
      return (await res.json()) as Status
    } catch {
      return undefined
    }
  })

  const dotColor = () => {
    const s = status()?.daemon.state
    if (s === "connected") return theme.success
    if (s === "disconnected") return theme.error
    return theme.warning
  }

  const Row = (props: { label: string; children: JSX.Element }) => (
    <box flexDirection="row" gap={1}>
      <box width={9} flexShrink={0}>
        <text fg={theme.textMuted}>{props.label}</text>
      </box>
      <text fg={theme.text} wrapMode="word">
        {props.children}
      </text>
    </box>
  )

  return (
    <box paddingLeft={2} paddingRight={2} gap={1} paddingBottom={1}>
      <box flexDirection="row" justifyContent="space-between">
        <text fg={theme.text} attributes={TextAttributes.BOLD}>
          Status
        </text>
        <text fg={theme.textMuted} onMouseUp={() => dialog.clear()}>
          esc
        </text>
      </box>
      <Show when={status()} fallback={<text fg={theme.textMuted}>Loading…</text>}>
        {(s) => (
          <box gap={1}>
            <Row label="App">
              <b>{s().app.name}</b>
              <Show when={s().app.version}>
                <span style={{ fg: theme.textMuted }}> v{s().app.version}</span>
              </Show>
            </Row>
            <Row label="Daemon">
              <span style={{ fg: dotColor() }}>●</span> {s().daemon.state}
              <span style={{ fg: theme.textMuted }}> {s().daemon.url}</span>
            </Row>
            <Row label="Context">
              {s().context.tokens.toLocaleString()} tokens
              <span style={{ fg: theme.textMuted }}>
                {" · "}
                {s().context.percent}% of {s().context.window.toLocaleString()}
              </span>
            </Row>
            <Show when={s().model}>
              <Row label="Model">{s().model}</Row>
            </Show>
            <Show when={s().modules.length > 0}>
              <box>
                <text fg={theme.text}>{s().modules.length} Modules</text>
                <For each={s().modules}>
                  {(m) => (
                    <box flexDirection="row" gap={1}>
                      <text flexShrink={0} style={{ fg: theme.success }}>
                        •
                      </text>
                      <text fg={theme.text}>
                        <b>{m}</b>
                      </text>
                    </box>
                  )}
                </For>
              </box>
            </Show>
          </box>
        )}
      </Show>
    </box>
  )
}
