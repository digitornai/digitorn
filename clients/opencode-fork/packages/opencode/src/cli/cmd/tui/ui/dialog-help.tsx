import { TextAttributes } from "@opentui/core"
import { createMemo, For } from "solid-js"
import { useTheme } from "@tui/context/theme"
import { useSync } from "@tui/context/sync"
import { useDialog } from "./dialog"
import { useBindings, useCommandShortcut, useCommandSlashes } from "../keymap"

export function DialogHelp() {
  const dialog = useDialog()
  const { theme } = useTheme()
  const sync = useSync()
  const palette = useCommandShortcut("command.palette.show")
  const slashes = useCommandSlashes()

  useBindings(() => ({
    bindings: [
      { key: "return", desc: "Close help", group: "Dialog", cmd: () => dialog.clear() },
      { key: "escape", desc: "Close help", group: "Dialog", cmd: () => dialog.clear() },
    ],
  }))

  // The slash commands actually available right now: the built-in ones (with a
  // slashName) + the daemon-backed server commands (/compact, /fork, /export, …).
  const commands = createMemo(() => {
    const map = new Map<string, string>()
    for (const c of sync.data.command ?? []) if (!map.has("/" + c.name)) map.set("/" + c.name, c.description ?? "")
    for (const s of slashes()) if (!map.has(s.display)) map.set(s.display, s.description ?? "")
    return [...map.entries()]
      .map(([name, description]) => ({ name, description }))
      .sort((a, b) => a.name.localeCompare(b.name))
  })

  return (
    <box paddingLeft={2} paddingRight={2} gap={1} paddingBottom={1}>
      <box flexDirection="row" justifyContent="space-between">
        <text attributes={TextAttributes.BOLD} fg={theme.text}>
          Help
        </text>
        <text fg={theme.textMuted} onMouseUp={() => dialog.clear()}>
          esc/enter
        </text>
      </box>
      <text fg={theme.textMuted}>
        Type <span style={{ fg: theme.text }}>/</span> in the composer, or press{" "}
        <span style={{ fg: theme.text }}>{palette()}</span> for all commands.
      </text>
      <box>
        <For each={commands()}>
          {(c) => (
            <box flexDirection="row" gap={1}>
              <box width={16} flexShrink={0}>
                <text fg={theme.text}>{c.name}</text>
              </box>
              <text fg={theme.textMuted} wrapMode="none">
                {c.description}
              </text>
            </box>
          )}
        </For>
      </box>
    </box>
  )
}
