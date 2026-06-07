import HomeFooter from "../feature-plugins/home/footer"
import HomeTips from "../feature-plugins/home/tips"
import SidebarContext from "../feature-plugins/sidebar/context"
import PromptContext from "../feature-plugins/prompt/context"
import SidebarMcp from "../feature-plugins/sidebar/mcp"
import SidebarLsp from "../feature-plugins/sidebar/lsp"
import SidebarTodo from "../feature-plugins/sidebar/todo"
import SidebarFiles from "../feature-plugins/sidebar/files"
import SidebarFooter from "../feature-plugins/sidebar/footer"
import PluginManager from "../feature-plugins/system/plugins"
import Notifications from "../feature-plugins/system/notifications"
import SessionV2Debug from "../feature-plugins/system/session-v2"
import WhichKey from "../feature-plugins/system/which-key"
import DiffViewer from "../feature-plugins/system/diff-viewer"
import SessionSwitcher from "../feature-plugins/session"
import { Flag } from "@opencode-ai/core/flag/flag"
import type { TuiPlugin, TuiPluginModule } from "@opencode-ai/plugin/tui"
import type { RuntimeFlags } from "@/effect/runtime-flags"

export type InternalTuiPlugin = Omit<TuiPluginModule, "id"> & {
  id: string
  tui: TuiPlugin
  enabled?: boolean
}

export function internalTuiPlugins(flags: Pick<RuntimeFlags.Info, "experimentalEventSystem">): InternalTuiPlugin[] {
  const all = [
    HomeFooter,
    HomeTips,
    SidebarContext,
    PromptContext,
    SidebarMcp,
    SidebarLsp,
    SidebarTodo,
    SidebarFiles,
    SidebarFooter,
    Notifications,
    PluginManager,
    WhichKey,
    DiffViewer,
    ...(flags.experimentalEventSystem ? [SessionV2Debug] : []),
    ...(Flag.OPENCODE_EXPERIMENTAL_SESSION_SWITCHER ? [SessionSwitcher] : []),
  ]
  // digitorn: drop the sidebar Context + LSP panels (user choice). Stock opencode
  // keeps them. The prompt-footer ctx gauge (PromptContext) is separate and stays.
  if (process.env.DIGITORN_URL)
    return all.filter((p) => p.id !== "internal:sidebar-context" && p.id !== "internal:sidebar-lsp")
  return all
}
