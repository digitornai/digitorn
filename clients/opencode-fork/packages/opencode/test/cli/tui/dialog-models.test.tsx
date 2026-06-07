/** @jsxImportSource @opentui/solid */
import { createDefaultOpenTuiKeymap } from "@opentui/keymap/opentui"
import { testRender, useRenderer } from "@opentui/solid"
import { expect, test } from "bun:test"
import path from "node:path"
import { mkdir } from "node:fs/promises"
import { onCleanup, type JSX } from "solid-js"
import { tmpdir } from "../../fixture/fixture"
import { createTuiResolvedConfig } from "../../fixture/tui-runtime"

const oneAgent = {
  byok: false,
  entry: "main",
  agents: [
    {
      agent: "main",
      role: "assistant",
      entry: true,
      kind: "chat",
      default: "claude-haiku-4-5",
      declared: ["gpt-4o"],
      override: "",
      model: "claude-haiku-4-5",
    },
  ],
  catalog: {
    chat: [
      { id: "claude-haiku-4-5", context: 200000, cat: "free" },
      { id: "gpt-4o", context: 128000, cat: "premium" },
    ],
  },
}

const twoAgents = {
  byok: false,
  entry: "main",
  agents: [
    { agent: "main", role: "assistant", entry: true, kind: "chat", default: "claude-haiku-4-5", model: "claude-haiku-4-5" },
    { agent: "explorer", role: "explorer", entry: false, kind: "chat", default: "claude-haiku-4-5", model: "claude-haiku-4-5" },
  ],
  catalog: { chat: [{ id: "claude-haiku-4-5", context: 200000 }] },
}

function sdkReturning(data: unknown) {
  let fetched = ""
  const sdk = {
    url: "http://test",
    fetch: (async (u: any) => {
      fetched = String(u)
      return new Response(JSON.stringify(data), { headers: { "content-type": "application/json" } })
    }) as unknown as typeof fetch,
  }
  return { sdk, fetched: () => fetched }
}

async function wait(fn: () => boolean, timeout = 5000) {
  const start = Date.now()
  while (!fn()) {
    if (Date.now() - start > timeout) throw new Error("timed out waiting for condition")
    await Bun.sleep(20)
  }
}

async function renderCaptured(tmpPath: string, factory: () => JSX.Element) {
  const { Global } = await import("@opencode-ai/core/global")
  const prev = { config: Global.Path.config, state: Global.Path.state }
  Global.Path.config = path.join(tmpPath, "config")
  Global.Path.state = path.join(tmpPath, "state")
  await mkdir(Global.Path.config, { recursive: true })
  await mkdir(Global.Path.state, { recursive: true })
  await Bun.write(path.join(Global.Path.state, "kv.json"), "{}")

  const [
    { DialogProvider },
    { ThemeProvider },
    { KVProvider },
    { TuiConfigProvider },
    { ToastProvider },
    { OpencodeKeymapProvider, registerOpencodeKeymap },
  ] = await Promise.all([
    import("../../../src/cli/cmd/tui/ui/dialog"),
    import("../../../src/cli/cmd/tui/context/theme"),
    import("../../../src/cli/cmd/tui/context/kv"),
    import("../../../src/cli/cmd/tui/context/tui-config"),
    import("../../../src/cli/cmd/tui/ui/toast"),
    import("../../../src/cli/cmd/tui/keymap"),
  ])

  const Harness = () => {
    const renderer = useRenderer()
    const keymap = createDefaultOpenTuiKeymap(renderer)
    const resolvedConfig = createTuiResolvedConfig({ keybinds: {}, leader_timeout: 1000 })
    const off = registerOpencodeKeymap(keymap, renderer, resolvedConfig)
    onCleanup(off)
    return (
      <OpencodeKeymapProvider keymap={keymap}>
        <TuiConfigProvider config={resolvedConfig}>
          <KVProvider>
            <ThemeProvider mode="dark">
              <ToastProvider>
                <DialogProvider>{factory()}</DialogProvider>
              </ToastProvider>
            </ThemeProvider>
          </KVProvider>
        </TuiConfigProvider>
      </OpencodeKeymapProvider>
    )
  }
  const app = await testRender(() => <Harness />, { kittyKeyboard: true })
  return { app, restore: () => ((Global.Path.config = prev.config), (Global.Path.state = prev.state)) }
}

async function open(sdk: { url: string; fetch: typeof fetch }, sessionID: string) {
  const { openModelsDialog } = await import("../../../src/cli/cmd/tui/component/dialog-models")
  let factory: (() => JSX.Element) | undefined
  const dialog = { replace: (f: () => JSX.Element) => (factory = f), clear: () => {} }
  await openModelsDialog(sdk, dialog, sessionID)
  return factory!
}

test("single agent opens the model picker with the gateway catalog (no loading loop)", async () => {
  await using tmp = await tmpdir()
  const { sdk, fetched } = sdkReturning(oneAgent)
  const factory = await open(sdk, "sess-1")
  expect(fetched()).toContain("/digitorn/session-model")
  expect(fetched()).toContain("session=sess-1")
  const { app, restore } = await renderCaptured(tmp.path, factory)
  try {
    await wait(() => app.captureCharFrame().includes("Use app default"))
    const frame = app.captureCharFrame()
    expect(frame).toContain("Model — assistant · main")
    expect(frame).toContain("claude-haiku-4-5")
    expect(frame).toContain("chat")
    expect(frame).not.toContain("Loading")
  } finally {
    app.renderer.destroy()
    restore()
  }
})

test("multiple agents open the agent picker first", async () => {
  await using tmp = await tmpdir()
  const { sdk } = sdkReturning(twoAgents)
  const factory = await open(sdk, "sess-2")
  const { app, restore } = await renderCaptured(tmp.path, factory)
  try {
    await wait(() => app.captureCharFrame().includes("explorer"))
    const frame = app.captureCharFrame()
    expect(frame).toContain("assistant")
    expect(frame).toContain("explorer")
  } finally {
    app.renderer.destroy()
    restore()
  }
})

test("no active session shows guidance instead of a picker", async () => {
  await using tmp = await tmpdir()
  const { sdk, fetched } = sdkReturning(oneAgent)
  const factory = await open(sdk, "")
  expect(fetched()).toBe("")
  const { app, restore } = await renderCaptured(tmp.path, factory)
  try {
    await wait(() => app.captureCharFrame().includes("Open a session"))
    expect(app.captureCharFrame()).toContain("Open a session")
  } finally {
    app.renderer.destroy()
    restore()
  }
})
