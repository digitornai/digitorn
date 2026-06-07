import { createMemo, onMount } from "solid-js"
import { useSync } from "@tui/context/sync"
import { DialogSelect, type DialogSelectOption } from "@tui/ui/dialog-select"
import type { TextPart } from "@opencode-ai/sdk/v2"
import { Locale } from "@/util/locale"
import { useSDK } from "@tui/context/sdk"
import { useRoute } from "@tui/context/route"
import { useDialog, type DialogContext } from "../../ui/dialog"
import { useToast } from "../../ui/toast"
import type { PromptInfo } from "@tui/component/prompt/history"
import { strip } from "@tui/component/prompt/part"

export function DialogForkFromTimeline(props: { sessionID: string; onMove: (messageID?: string) => void }) {
  const sync = useSync()
  const dialog = useDialog()
  const sdk = useSDK()
  const route = useRoute()
  const toast = useToast()

  onMount(() => {
    dialog.setSize("large")
  })

  const options = createMemo((): DialogSelectOption<string | undefined>[] => {
    const messages = sync.data.message[props.sessionID] ?? []
    const fullSession = {
      title: "Full session",
      value: undefined,
      onSelect: async (dialog: DialogContext) => {
        try {
          const forked = await sdk.client.session.fork({ sessionID: props.sessionID })
          if (!forked.data?.id) throw new Error(forked.error ? JSON.stringify(forked.error) : "no session returned")
          dialog.clear()
          // The forked session is brand new — not in the synced store yet. Pre-sync
          // it (session + messages) BEFORE navigating so the route renders it
          // immediately instead of a blank view that depends on an async reload.
          await sync.session.sync(forked.data.id).catch(() => {})
          route.navigate({ sessionID: forked.data.id, type: "session" })
        } catch (e: any) {
          toast.show({ variant: "error", message: `Fork failed: ${String(e?.message ?? e)}` })
          dialog.clear()
        }
      },
    } satisfies DialogSelectOption<string | undefined>
    const result = [] as DialogSelectOption<string | undefined>[]
    for (const message of messages) {
      if (message.role !== "user") continue
      const part = (sync.data.part[message.id] ?? []).find(
        (x) => x.type === "text" && !x.synthetic && !x.ignored,
      ) as TextPart
      if (!part) continue
      result.push({
        title: part.text.replace(/\n/g, " "),
        value: message.id,
        footer: Locale.time(message.time.created),
        onSelect: async (dialog) => {
          try {
            const forked = await sdk.client.session.fork({
              sessionID: props.sessionID,
              messageID: message.id,
            })
            if (!forked.data?.id) throw new Error(forked.error ? JSON.stringify(forked.error) : "no session returned")
            const parts = sync.data.part[message.id] ?? []
            const prompt = parts.reduce(
              (agg, part) => {
                if (part.type === "text") {
                  if (!part.synthetic) agg.input += part.text
                }
                if (part.type === "file") agg.parts.push(strip(part))
                return agg
              },
              { input: "", parts: [] as PromptInfo["parts"] },
            )
            dialog.clear()
            await sync.session.sync(forked.data.id).catch(() => {}) // pre-load the new session so the view renders it
            route.navigate({ sessionID: forked.data.id, type: "session", prompt })
          } catch (e: any) {
            toast.show({ variant: "error", message: `Fork failed: ${String(e?.message ?? e)}` })
            dialog.clear()
          }
        },
      })
    }
    return [fullSession, ...result.reverse()]
  })

  return <DialogSelect onMove={(option) => props.onMove(option.value)} title="Fork session" options={options()} />
}
