import { useTheme } from "../context/theme"
import { TextAttributes } from "@opentui/core"
import { createSignal } from "solid-js"

export interface TodoItemProps {
  status: string
  content: string
  // Available content columns. When set, a todo longer than this renders on a
  // SINGLE truncated line ("…") instead of wrapping, and clicking it unfolds the
  // full text. Omit it (e.g. the wide inline block) to keep the wrapping default.
  width?: number
}

// Compact todo row: a single-char status dot (○ pending · ◐ in-progress · ●
// done) instead of [ ]/[•]/[✓], and DIM text for a lighter, smaller feel
// (terminals can't shrink the font, so DIM is the closest).
export function TodoItem(props: TodoItemProps) {
  const { theme } = useTheme()
  const [expanded, setExpanded] = createSignal(false)
  const icon = props.status === "completed" ? "●" : props.status === "in_progress" ? "◐" : "○"
  const fg = props.status === "in_progress" ? theme.warning : theme.textMuted

  const overflows = () => props.width !== undefined && props.content.length > props.width
  const collapsed = () => overflows() && !expanded()
  const display = () =>
    collapsed() ? props.content.slice(0, Math.max(1, props.width! - 1)).trimEnd() + "…" : props.content

  return (
    <box flexDirection="row" gap={1} onMouseDown={() => overflows() && setExpanded((x) => !x)}>
      <text flexShrink={0} attributes={TextAttributes.DIM} style={{ fg }}>
        {icon}
      </text>
      <text
        flexGrow={1}
        wrapMode={collapsed() ? "none" : "word"}
        attributes={TextAttributes.DIM}
        style={{ fg }}
      >
        {display()}
      </text>
    </box>
  )
}
