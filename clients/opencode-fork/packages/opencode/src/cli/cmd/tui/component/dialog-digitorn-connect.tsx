import { onCleanup } from "solid-js"
import { TextAttributes } from "@opentui/core"
import { DialogSelect } from "@tui/ui/dialog-select"
import { useDialog } from "@tui/ui/dialog"
import { useSync } from "@tui/context/sync"
import { useTheme } from "../context/theme"
import { useToast } from "../ui/toast"
import { Link } from "../ui/link"
import { AUTH_PROVIDERS, startDigitornLogin, type AuthProvider } from "../context/digitorn-auth"
import { applyDigitornCredentials } from "../context/digitorn"

const PROVIDER_LABEL: Record<AuthProvider, string> = {
  google: "Google",
  microsoft: "Microsoft",
  azure: "Azure",
}

// DialogDigitornConnect replaces opencode's provider-list for /connect when
// DIGITORN_URL is set : pick an upstream identity provider, then run the same
// browser bounce flow the old `digitorn login` did. On success the credentials
// are persisted AND swapped into the running adapter (no restart needed).
export function DialogDigitornConnect() {
  const dialog = useDialog()
  const options = AUTH_PROVIDERS.map((provider) => ({
    title: PROVIDER_LABEL[provider],
    value: provider,
    onSelect() {
      dialog.replace(() => <WaitingForBrowser provider={provider} />)
    },
  }))
  return <DialogSelect title="Sign in to digitorn" options={options} />
}

function WaitingForBrowser(props: { provider: AuthProvider }) {
  const { theme } = useTheme()
  const dialog = useDialog()
  const toast = useToast()
  const sync = useSync()

  const handle = startDigitornLogin({ provider: props.provider })
  handle.done
    .then(async (creds) => {
      applyDigitornCredentials(creds)
      await sync.bootstrap().catch(() => {})
      toast.show({ variant: "info", message: `Signed in as ${creds.email || creds.user_id || "digitorn"}` })
      dialog.clear()
    })
    .catch((e: Error) => {
      if (e.message !== "cancelled") toast.show({ variant: "error", message: `Sign-in failed: ${e.message}` })
      dialog.clear()
    })
  onCleanup(() => handle.cancel()) // esc / unmount tears down the localhost listener

  return (
    <box paddingLeft={2} paddingRight={2} gap={1} paddingBottom={1}>
      <box flexDirection="row" justifyContent="space-between">
        <text attributes={TextAttributes.BOLD} fg={theme.text}>
          Sign in with {PROVIDER_LABEL[props.provider]}
        </text>
        <text fg={theme.textMuted} onMouseUp={() => dialog.clear()}>
          esc
        </text>
      </box>
      <box gap={1}>
        <text fg={theme.textMuted}>Your browser should open. If not, open this URL:</text>
        <Link href={handle.url} fg={theme.primary} />
      </box>
      <text fg={theme.textMuted}>Waiting for authorization…</text>
    </box>
  )
}
