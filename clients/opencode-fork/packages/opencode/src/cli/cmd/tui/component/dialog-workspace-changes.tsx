import { TextAttributes } from "@opentui/core"
import { type JSX } from "solid-js"
import { DialogSelect, type DialogSelectOption } from "@tui/ui/dialog-select"
import { useDialog } from "@tui/ui/dialog"
import { useToast } from "@tui/ui/toast"
import { useSDK } from "@tui/context/sdk"
import { useTheme } from "../context/theme"
import { Spinner } from "./spinner"

type Change = { file: string; additions?: number; deletions?: number; status?: string }
type Hunk = { hash: string; header: string; additions?: number; deletions?: number }

type SdkLike = { fetch: typeof fetch; url: string }
type DialogLike = { replace: (factory: () => JSX.Element) => void; clear: () => void }

// Fetch the pending change set BEFORE opening, never inside the dialog's reactive
// render — fetch-in-render loops on this opentui host (the RAM-explosion bug; see
// dialog-skills). /session/{id}/diff already returns {file,additions,deletions} for
// the live change set, so we reuse it (no new route).
async function fetchChanges(sdk: SdkLike, sid: string): Promise<Change[]> {
  try {
    const res = await sdk.fetch(`${sdk.url}/session/${encodeURIComponent(sid)}/diff`)
    const data = (await res.json()) as Change[]
    return Array.isArray(data) ? data : []
  } catch {
    return []
  }
}

export async function openWorkspaceChanges(sdk: SdkLike, dialog: DialogLike, sid: string) {
  if (!sid) {
    dialog.clear()
    return
  }
  const data = await fetchChanges(sdk, sid)
  dialog.replace(() => <DialogWorkspaceChanges data={data} sid={sid} />)
}

export function DialogWorkspaceChanges(props: { data: Change[]; sid: string }) {
  const sdk = useSDK()
  const dialog = useDialog()
  const toast = useToast()
  const { theme } = useTheme()

  const refresh = () => void openWorkspaceChanges(sdk, dialog, props.sid)

  const post = async (action: string, body: Record<string, unknown>, okMsg: string) => {
    // Swap to a loading state immediately — the shadow-git commit/revert can take a
    // few hundred ms on large files; this gives instant feedback and blocks a
    // double-action on the now-stale menu. refresh() restores the live list.
    dialog.replace(() => (
      <Shell>
        <Spinner>Applying…</Spinner>
      </Shell>
    ))
    try {
      const res = await sdk.fetch(`${sdk.url}/digitorn/workspace/${action}`, {
        method: "POST",
        body: JSON.stringify({ session: props.sid, ...body }),
      })
      const j = await res.json().catch(() => ({}))
      if (j?.error) toast.show({ message: String(j.error), variant: "error" })
      else toast.show({ message: okMsg, variant: "success" })
    } catch (e: any) {
      toast.show({ message: String(e?.message ?? e), variant: "error" })
    }
    refresh()
  }

  const fileActions = (f: Change) =>
    dialog.replace(() => (
      <DialogSelect
        title={f.file}
        options={[
          { title: "Approve", value: "approve", description: "Commit this file to the workspace baseline" },
          { title: "Reject", value: "reject", description: "Revert this file to baseline" },
          { title: "Review hunks…", value: "hunks", description: "Approve / reject individual hunks" },
          { title: "Back", value: "back" },
        ]}
        onSelect={(o) => {
          if (o.value === "approve") void post("approve", { path: f.file }, `Approved ${f.file}`)
          else if (o.value === "reject") void post("reject", { path: f.file }, `Reverted ${f.file}`)
          else if (o.value === "hunks") void openHunks(f.file)
          else refresh()
        }}
      />
    ))

  const fetchHunks = async (file: string): Promise<Hunk[]> => {
    try {
      const res = await sdk.fetch(
        `${sdk.url}/digitorn/workspace/hunks?session=${encodeURIComponent(props.sid)}&path=${encodeURIComponent(file)}`,
      )
      const j = (await res.json()) as { hunks?: Hunk[] }
      return Array.isArray(j.hunks) ? j.hunks : []
    } catch {
      return []
    }
  }

  const stat = (a?: number, d?: number) => [a ? `+${a}` : "", d ? `-${d}` : ""].filter(Boolean).join(" ")

  const hunkActions = (file: string, h: Hunk) =>
    dialog.replace(() => (
      <DialogSelect
        title={h.header}
        options={[
          { title: "Approve hunk", value: "approve", description: "Commit only this hunk to baseline" },
          { title: "Reject hunk", value: "reject", description: "Revert only this hunk" },
          { title: "Back", value: "back" },
        ]}
        onSelect={(o) => {
          if (o.value === "approve") void post("approve-hunks", { path: file, hunks: [h.hash] }, "Approved hunk")
          else if (o.value === "reject") void post("reject-hunks", { path: file, hunks: [h.hash] }, "Reverted hunk")
          else void openHunks(file)
        }}
      />
    ))

  const openHunks = async (file: string) => {
    const hunks = await fetchHunks(file)
    if (hunks.length === 0) {
      toast.show({ message: "No hunks (file may be binary or already resolved)", variant: "info" })
      refresh()
      return
    }
    dialog.replace(() => (
      <DialogSelect
        title={`Hunks · ${file}`}
        options={hunks.map((h, i) => ({ title: h.header, value: String(i), footer: stat(h.additions, h.deletions) }))}
        onSelect={(o) => {
          const h = hunks[Number(o.value)]
          if (h) hunkActions(file, h)
        }}
      />
    ))
  }

  const Shell = (p: { children: JSX.Element }) => (
    <box paddingLeft={2} paddingRight={2} gap={1} paddingBottom={1}>
      <box flexDirection="row" justifyContent="space-between">
        <text fg={theme.text} attributes={TextAttributes.BOLD}>
          Changes
        </text>
        <text fg={theme.textMuted} onMouseUp={() => dialog.clear()}>
          esc
        </text>
      </box>
      {p.children}
    </box>
  )

  if (props.data.length === 0)
    return (
      <Shell>
        <text fg={theme.textMuted}>No pending changes.</text>
      </Shell>
    )

  const opts: DialogSelectOption<string>[] = [
    { title: "Approve all", value: "__approve_all__", category: "All" },
    { title: "Reject all", value: "__reject_all__", category: "All" },
  ]
  for (const f of props.data) {
    const stat = [f.additions ? `+${f.additions}` : "", f.deletions ? `-${f.deletions}` : ""].filter(Boolean).join(" ")
    opts.push({ title: f.file, value: "file:" + f.file, footer: stat, category: "Files" })
  }

  return (
    <DialogSelect
      title="Changes"
      options={opts}
      onSelect={(o) => {
        if (o.value === "__approve_all__") void post("approve-all", {}, "Approved all changes")
        else if (o.value === "__reject_all__") void post("reject-all", {}, "Reverted all changes")
        else if (o.value.startsWith("file:")) {
          const f = props.data.find((x) => x.file === o.value.slice(5))
          if (f) fileActions(f)
        }
      }}
    />
  )
}
