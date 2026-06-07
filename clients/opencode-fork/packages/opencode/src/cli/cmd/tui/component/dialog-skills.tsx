import { TextAttributes } from "@opentui/core"
import { type JSX } from "solid-js"
import { DialogSelect, type DialogSelectOption } from "@tui/ui/dialog-select"
import { DialogPrompt } from "@tui/ui/dialog-prompt"
import { useDialog } from "@tui/ui/dialog"
import { useToast } from "@tui/ui/toast"
import { useSDK } from "@tui/context/sdk"
import { useTheme } from "../context/theme"

type UserSkill = { id: string; name: string; description: string; instructions: string }
type SkillsResponse = {
  appSkills: { command: string; description: string }[]
  userSkills: UserSkill[]
  allow: boolean
  error?: string
}

type SdkLike = { fetch: typeof fetch; url: string }
type DialogLike = { replace: (factory: () => JSX.Element) => void; clear: () => void }

// Fetch the skills ONCE, then render. No fetch/signal lives inside the component
// render, so it can NEVER enter a reactive re-fetch loop (the bug that exploded
// RAM). Caches the result; invalidated after a write so the next open is fresh.
let skillsCache: SkillsResponse | undefined
async function fetchSkills(sdk: SdkLike): Promise<SkillsResponse> {
  if (skillsCache) return skillsCache
  try {
    const res = await sdk.fetch(`${sdk.url}/digitorn/skills`)
    skillsCache = (await res.json()) as SkillsResponse
  } catch (e: any) {
    skillsCache = { appSkills: [], userSkills: [], allow: false, error: String(e?.message ?? e) }
  }
  return skillsCache
}

// THE entry point: fetch, then open the static dialog. Called by the /skill
// command and after every CRUD action.
export async function openSkillsDialog(sdk: SdkLike, dialog: DialogLike) {
  const data = await fetchSkills(sdk)
  dialog.replace(() => <DialogSkills data={data} />)
}

// /use_skill picker: fetch the available skills (user + app), let the user pick
// one, and hand the chosen name back via onPick (the prompt then prefills the
// composer with "/use_skill <name> "). Fetch-before-open → static → no loop.
export async function openUseSkillPicker(sdk: SdkLike, dialog: DialogLike, onPick: (name: string) => void) {
  skillsCache = undefined
  const data = await fetchSkills(sdk)
  const opts: DialogSelectOption<string>[] = []
  for (const s of data.userSkills ?? []) if (s.name) opts.push({ title: s.name, value: s.name, description: s.description, footer: "you", category: "Your skills" })
  for (const s of data.appSkills ?? []) {
    const name = s.command.replace(/^\//, "")
    if (name) opts.push({ title: name, value: name, description: s.description, footer: "app", category: "App skills" })
  }
  if (opts.length === 0) {
    dialog.clear()
    return
  }
  dialog.replace(() => (
    <DialogSelect
      title="Use a skill"
      options={opts}
      onSelect={(o) => {
        dialog.clear()
        onPick(o.value)
      }}
    />
  ))
}

export function DialogSkills(props: { data: SkillsResponse }) {
  const sdk = useSDK()
  const dialog = useDialog()
  const toast = useToast()
  const { theme } = useTheme()

  const refresh = async () => {
    skillsCache = undefined
    await openSkillsDialog(sdk, dialog)
  }

  const write = async (method: string, path: string, body: unknown | undefined, okMsg: string) => {
    try {
      const res = await sdk.fetch(`${sdk.url}${path}`, { method, ...(body ? { body: JSON.stringify(body) } : {}) })
      const j = await res.json().catch(() => ({}))
      if (j?.error) toast.show({ message: String(j.error), variant: "error" })
      else toast.show({ message: okMsg, variant: "success" })
    } catch (e: any) {
      toast.show({ message: String(e?.message ?? e), variant: "error" })
    }
    await refresh()
  }

  const form = async (titlePrefix: string, seed?: UserSkill) => {
    const name = await DialogPrompt.show(dialog, `${titlePrefix} — name`, { value: seed?.name, placeholder: "commit" })
    if (name === null) return null
    const description = await DialogPrompt.show(dialog, `${titlePrefix} — description`, { value: seed?.description, placeholder: "Short summary" })
    if (description === null) return null
    const instructions = await DialogPrompt.show(dialog, `${titlePrefix} — instructions`, { value: seed?.instructions, placeholder: "Markdown the agent must follow…" })
    if (instructions === null) return null
    return { name: name.trim(), description: description.trim(), instructions }
  }

  const createFlow = async () => {
    const v = await form("New skill")
    if (!v || !v.name) return void refresh()
    await write("POST", "/digitorn/skills", v, `Skill "${v.name}" created`)
  }

  const editFlow = async (sk: UserSkill) => {
    const v = await form(`Edit ${sk.name}`, sk)
    if (!v || !v.name) return void refresh()
    await write("PATCH", `/digitorn/skills/${encodeURIComponent(sk.id)}`, v, `Skill "${v.name}" saved`)
  }

  const deleteFlow = (sk: UserSkill) =>
    dialog.replace(() => (
      <DialogSelect
        title={`Delete "${sk.name}"?`}
        options={[
          { title: "Delete", value: "yes" },
          { title: "Cancel", value: "no" },
        ]}
        onSelect={(o) => {
          if (o.value === "yes") void write("DELETE", `/digitorn/skills/${encodeURIComponent(sk.id)}`, undefined, `Deleted "${sk.name}"`)
          else void refresh()
        }}
      />
    ))

  const userActions = (sk: UserSkill) =>
    dialog.replace(() => (
      <DialogSelect
        title={sk.name}
        options={[
          { title: "Edit", value: "edit", description: sk.description },
          { title: "Delete", value: "delete" },
          { title: "Back", value: "back" },
        ]}
        onSelect={(o) => {
          if (o.value === "edit") void editFlow(sk)
          else if (o.value === "delete") deleteFlow(sk)
          else void refresh()
        }}
      />
    ))

  // Static option list — computed once from the prop, NOT a reactive memo.
  const opts: DialogSelectOption<string>[] = []
  if (props.data.allow) opts.push({ title: "+ New skill", value: "__new__", category: "Manage" })
  for (const s of props.data.userSkills ?? []) opts.push({ title: s.name, value: "user:" + s.id, description: s.description, footer: "you", category: "Your skills" })
  for (const s of props.data.appSkills ?? []) opts.push({ title: s.command, value: "app:" + s.command, description: s.description, footer: "app", category: "App skills", disabled: true })

  const Shell = (p: { children: JSX.Element }) => (
    <box paddingLeft={2} paddingRight={2} gap={1} paddingBottom={1}>
      <box flexDirection="row" justifyContent="space-between">
        <text fg={theme.text} attributes={TextAttributes.BOLD}>
          Skills
        </text>
        <text fg={theme.textMuted} onMouseUp={() => dialog.clear()}>
          esc
        </text>
      </box>
      {p.children}
    </box>
  )

  if (props.data.error)
    return (
      <Shell>
        <text fg={theme.error}>Failed to load skills</text>
        <text fg={theme.textMuted}>{props.data.error}</text>
      </Shell>
    )
  if (opts.length === 0)
    return (
      <Shell>
        <text fg={theme.textMuted}>No skills available for this app.</text>
      </Shell>
    )

  return (
    <DialogSelect
      title="Skills"
      options={opts}
      onSelect={(o) => {
        if (o.value === "__new__") void createFlow()
        else if (o.value.startsWith("user:")) {
          const sk = (props.data.userSkills ?? []).find((s) => s.id === o.value.slice(5))
          if (sk) userActions(sk)
        }
      }}
    />
  )
}
