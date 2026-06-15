import type { PieceLoader } from './loader.ts'
import { resolveAuthValue, makeActionContext, extractPropsAndAuth } from './context.ts'

// parseToolName splits "ap_{piece}__{action}" into piece and action parts.
export function parseToolName(name: string): { piece: string; action: string } | null {
  if (!name.startsWith('ap_')) return null
  const rest = name.slice(3) // strip "ap_"
  const sep = rest.indexOf('__')
  if (sep < 0) return null
  return { piece: rest.slice(0, sep), action: rest.slice(sep + 2) }
}

export async function executeTool(
  toolName: string,
  args: Record<string, unknown>,
  loader: PieceLoader,
): Promise<{ success: boolean; data?: unknown; error?: string; display?: string }> {
  const parsed = parseToolName(toolName)
  if (!parsed) return { success: false, error: `invalid tool name: ${toolName}` }

  const piece = loader.getPiece(parsed.piece)
  if (!piece) return { success: false, error: `piece "${parsed.piece}" is not installed` }

  const actionDef = piece.metadata.actions[parsed.action]
  if (!actionDef) return { success: false, error: `action "${parsed.action}" not found in piece "${parsed.piece}"` }

  const { auth: rawAuth, sessionId, propsValue } = extractPropsAndAuth(args)
  const auth = resolveAuthValue(rawAuth)
  const ctx = makeActionContext(auth, propsValue, sessionId, parsed.action)

  try {
    const result = await actionDef.run(ctx)
    return { success: true, data: result }
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err)
    process.stderr.write(`pieces-bridge: ${toolName} failed: ${msg}\n`)
    return { success: false, error: msg }
  }
}
