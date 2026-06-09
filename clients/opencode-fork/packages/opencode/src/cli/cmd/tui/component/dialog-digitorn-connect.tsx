import { TextAttributes } from "@opentui/core"
import { DialogSelect } from "@tui/ui/dialog-select"
import { useDialog } from "@tui/ui/dialog"
import { useSync } from "@tui/context/sync"
import { useTheme } from "../context/theme"
import { useToast } from "../ui/toast"
import { Link } from "../ui/link"
import { AUTH_PROVIDERS, startDigitornLogin, refreshDigitornAuth, type AuthProvider } from "../context/digitorn-auth"
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
//
// The login is started ONCE in onSelect (NOT in the waiting component's render) :
// the localhost callback listener must keep the SAME port for the whole flow, so
// it can never be torn down + re-bound by an @opentui re-render of the waiting view.
export function DialogDigitornConnect() {
  const dialog = useDialog()
  const toast = useToast()
  const sync = useSync()
  const options = AUTH_PROVIDERS.map((provider) => ({
    title: PROVIDER_LABEL[provider],
    value: provider,
    onSelect() {
      const handle = startDigitornLogin({ provider })
      handle.done
        .then(async (creds) => {
          applyDigitornCredentials(creds)
          refreshDigitornAuth()
          await sync.bootstrap().catch(() => {})
          toast.show({ variant: "info", message: `Signed in as ${creds.email || creds.user_id || "digitorn"}` })
          dialog.clear()
        })
        .catch((e: Error) => {
          if (e.message !== "cancelled") toast.show({ variant: "error", message: `Sign-in failed: ${e.message}` })
          dialog.clear()
        })
      dialog.replace(() => (
        <WaitingForBrowser provider={provider} url={handle.url} onCancel={() => handle.cancel()} />
      ))
    },
  }))
  return <DialogSelect title="Sign in to digitorn" options={options} />
}

// Purely presentational : it only shows the URL + an esc affordance. The login
// handle lives in the parent's onSelect closure, so a re-render here never touches
// the listener. esc cancels the flow (tears down the listener) and closes.
function WaitingForBrowser(props: { provider: AuthProvider; url: string; onCancel: () => void }) {
  const { theme } = useTheme()
  const dialog = useDialog()
  const close = () => {
    props.onCancel()
    dialog.clear()
  }
  return (
    <box paddingLeft={2} paddingRight={2} gap={1} paddingBottom={1}>
      <box flexDirection="row" justifyContent="space-between">
        <text attributes={TextAttributes.BOLD} fg={theme.text}>
          Sign in with {PROVIDER_LABEL[props.provider]}
        </text>
        <text fg={theme.textMuted} onMouseUp={close}>
          esc
        </text>
      </box>
      <box gap={1}>
        <text fg={theme.textMuted}>Your browser should open. If not, open this URL:</text>
        <Link href={props.url} fg={theme.primary} />
      </box>
      <text fg={theme.textMuted}>Waiting for authorization…</text>
    </box>
  )
}
