import { createResource, onMount } from "solid-js"
import { DialogSelect } from "@tui/ui/dialog-select"
import { useDialog } from "@tui/ui/dialog"
import { useRoute } from "@tui/context/route"
import { useSDK } from "../context/sdk"

const truncate = (s: string, n: number) => (s.length > n ? s.slice(0, n - 1).trimEnd() + "…" : s)

// Digitorn is a multi-app platform: this picker lists the installed digitorn apps
// (chat-claude, claude-code, web-probe, …) and switches the live one. The adapter
// (/digitorn/app) re-points currentApp and re-bootstraps the session list.
type App = { id: string; name: string; description: string; category: string; current: boolean }

export function DialogApps() {
  const sdk = useSDK()
  const dialog = useDialog()
  const { navigate } = useRoute()

  onMount(() => dialog.setSize("large")) // wider modal for the app list

  const [apps] = createResource(async () => {
    try {
      const res = await sdk.fetch(`${sdk.url}/digitorn/apps`)
      return (await res.json()) as App[]
    } catch {
      return [] as App[]
    }
  })

  const options = () =>
    (apps() ?? []).map((a) => ({
      title: a.name,
      value: a.id,
      description: truncate(a.description || a.id, 64), // keep long blurbs on one line
      category: a.current ? "Current" : a.category || "Apps",
    }))

  return (
    <DialogSelect
      title="Switch digitorn app"
      options={options()}
      onSelect={async (opt) => {
        try {
          await sdk.fetch(`${sdk.url}/digitorn/app`, {
            method: "POST",
            body: JSON.stringify({ app_id: opt.value }),
          })
        } catch {}
        dialog.clear()
        navigate({ type: "home" })
      }}
    />
  )
}
