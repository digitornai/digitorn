import type { TuiPlugin, TuiPluginApi } from "@opencode-ai/plugin/tui"
import type { InternalTuiPlugin } from "../../plugin/internal"
import { createMemo, createResource, createSignal, onCleanup, onMount, Show } from "solid-js"
import { Global } from "@opencode-ai/core/global"
import { useSDK } from "@tui/context/sdk"
import { digitornAuthState } from "../../context/digitorn-auth"

const DIGITORN = Boolean(process.env.DIGITORN_URL)

const id = "internal:sidebar-footer"

function View(props: { api: TuiPluginApi }) {
  const theme = () => props.api.theme.current
  const sdk = useSDK()
  // The footer identifies the RUNNING digitorn app (chat-simple, claude-code, …)
  // — digitorn is the platform, the app is what the user is actually in.
  const [digitornApps] = createResource(async () => {
    try {
      const res = await sdk.fetch(`${sdk.url}/digitorn/apps`)
      return (await res.json()) as Array<{ id: string; name: string; color?: string; current?: boolean; version?: string }>
    } catch {
      return [] as Array<{ id: string; name: string; color?: string; current?: boolean; version?: string }>
    }
  })
  const currentApp = createMemo(() => (digitornApps() ?? []).find((a) => a.current))
  // The dot before the app name is the daemon CONNECTION indicator, like the Go
  // CLI's socket-driven status dot: green=connected, yellow=connecting, red=down.
  // PUSHED, not polled — the adapter broadcasts digitorn.connection on every
  // socket transition through the SAME event stream the chat uses. We take one
  // snapshot on mount, then live updates.
  const [conn, setConn] = createSignal("connecting")
  onMount(() => {
    sdk
      .fetch(`${sdk.url}/digitorn/connection`)
      .then((r) => r.json())
      .then((j: any) => setConn(j?.state ?? "connecting"))
      .catch(() => {})
    const unsub = sdk.event.on("event", (ev: any) => {
      if (ev?.payload?.type === "digitorn.connection") setConn(ev.payload.properties?.state ?? "connecting")
    })
    onCleanup(() => unsub())
  })
  const dotColor = () => {
    const s = conn()
    if (s === "connected") return theme().success
    if (s === "disconnected") return theme().error
    return theme().warning
  }
  const has = createMemo(() =>
    props.api.state.provider.some(
      (item) => item.id !== "opencode" || Object.values(item.models).some((model) => model.cost?.input !== 0),
    ),
  )
  const done = createMemo(() => props.api.kv.get("dismissed_getting_started", false))
  // Stock opencode's provider banner is about opencode's own providers — irrelevant
  // when digitorn drives the models, so it's suppressed here in favour of the
  // digitorn sign-in banner below.
  const show = createMemo(() => !DIGITORN && !has() && !done())
  const auth = createMemo(() => digitornAuthState())
  const showSignIn = createMemo(() => DIGITORN && !auth().connected)
  const path = createMemo(() => {
    const dir = props.api.state.path.directory || process.cwd()
    const out = dir.replace(Global.Path.home, "~")
    const text = props.api.state.vcs?.branch ? out + ":" + props.api.state.vcs.branch : out
    const list = text.split("/")
    return {
      parent: list.slice(0, -1).join("/"),
      name: list.at(-1) ?? "",
    }
  })

  return (
    <box gap={1}>
      <Show when={showSignIn()}>
        <box
          backgroundColor={theme().backgroundElement}
          paddingTop={1}
          paddingBottom={1}
          paddingLeft={2}
          paddingRight={2}
          flexDirection="row"
          gap={1}
        >
          <text flexShrink={0} fg={theme().warning}>
            ●
          </text>
          <box flexGrow={1} gap={1}>
            <text fg={theme().text}>
              <b>{auth().expired ? "Session expired" : "Not signed in"}</b>
            </text>
            <text fg={theme().textMuted}>Sign in to digitorn to start a session.</text>
            <box flexDirection="row" gap={1} justifyContent="space-between">
              <text fg={theme().text}>Sign in</text>
              <text fg={theme().textMuted}>/connect</text>
            </box>
          </box>
        </box>
      </Show>
      <Show when={show()}>
        <box
          backgroundColor={theme().backgroundElement}
          paddingTop={1}
          paddingBottom={1}
          paddingLeft={2}
          paddingRight={2}
          flexDirection="row"
          gap={1}
        >
          <text flexShrink={0} fg={theme().text}>
            ⬖
          </text>
          <box flexGrow={1} gap={1}>
            <box flexDirection="row" justifyContent="space-between">
              <text fg={theme().text}>
                <b>Getting started</b>
              </text>
              <text fg={theme().textMuted} onMouseDown={() => props.api.kv.set("dismissed_getting_started", true)}>
                ✕
              </text>
            </box>
            <text fg={theme().textMuted}>OpenCode includes free models so you can start immediately.</text>
            <text fg={theme().textMuted}>
              Connect from 75+ providers to use other models, including Claude, GPT, Gemini etc
            </text>
            <box flexDirection="row" gap={1} justifyContent="space-between">
              <text fg={theme().text}>Connect provider</text>
              <text fg={theme().textMuted}>/connect</text>
            </box>
          </box>
        </box>
      </Show>
      <text>
        <span style={{ fg: theme().textMuted }}>{path().parent}/</span>
        <span style={{ fg: theme().text }}>{path().name}</span>
      </text>
      <text fg={theme().textMuted}>
        <span style={{ fg: dotColor() }}>●</span>{" "}
        <span style={{ fg: theme().text }}>
          <b>{currentApp()?.name ?? "Digitorn"}</b>
        </span>
        <Show when={currentApp()?.version}>
          <span style={{ fg: theme().textMuted }}> v{currentApp()!.version}</span>
        </Show>
      </text>
      <Show when={DIGITORN && auth().connected && (auth().name || auth().email)}>
        <text fg={theme().textMuted}>Signed in as {auth().name || auth().email}</text>
      </Show>
    </box>
  )
}

const tui: TuiPlugin = async (api) => {
  api.slots.register({
    order: 100,
    slots: {
      sidebar_footer() {
        return <View api={api} />
      },
    },
  })
}

const plugin: InternalTuiPlugin = {
  id,
  tui,
}

export default plugin
