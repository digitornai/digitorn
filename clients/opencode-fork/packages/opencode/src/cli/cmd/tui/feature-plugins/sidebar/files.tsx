import type { TuiPlugin, TuiPluginApi } from "@opencode-ai/plugin/tui"
import type { InternalTuiPlugin } from "../../plugin/internal"
import { createMemo, For, Show, createSignal } from "solid-js"

const id = "internal:sidebar-files"

function View(props: { api: TuiPluginApi; session_id: string }) {
  const [open, setOpen] = createSignal(true)
  const theme = () => props.api.theme.current
  const list = createMemo(() => props.api.state.session.diff(props.session_id))
  const total = createMemo(() =>
    list().reduce((a, f) => ({ add: a.add + (f.additions ?? 0), del: a.del + (f.deletions ?? 0) }), { add: 0, del: 0 }),
  )

  return (
    <Show when={list().length > 0}>
      <box>
        <box
          flexDirection="row"
          gap={1}
          justifyContent="space-between"
          onMouseDown={() => list().length > 2 && setOpen((x) => !x)}
        >
          <box flexDirection="row" gap={1}>
            <Show when={list().length > 2}>
              <text fg={theme().text}>{open() ? "▼" : "▶"}</text>
            </Show>
            <text fg={theme().text}>
              <b>Modified Files</b>
            </text>
          </box>
          <box flexDirection="row" gap={1} flexShrink={0}>
            <Show when={total().add}>
              <text fg={theme().diffAdded}>+{total().add}</text>
            </Show>
            <Show when={total().del}>
              <text fg={theme().diffRemoved}>-{total().del}</text>
            </Show>
          </box>
        </box>
        <Show when={list().length <= 2 || open()}>
          <For each={list()}>
            {(item) => (
              <box flexDirection="row" gap={1} justifyContent="space-between">
                <text fg={theme().textMuted} wrapMode="none">
                  {item.file}
                </text>
                <box flexDirection="row" gap={1} flexShrink={0}>
                  <Show when={item.additions}>
                    <text fg={theme().diffAdded}>+{item.additions}</text>
                  </Show>
                  <Show when={item.deletions}>
                    <text fg={theme().diffRemoved}>-{item.deletions}</text>
                  </Show>
                </box>
              </box>
            )}
          </For>
        </Show>
      </box>
    </Show>
  )
}

const tui: TuiPlugin = async (api) => {
  api.slots.register({
    order: 500,
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
