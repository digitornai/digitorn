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

  const piece = await loader.getPiece(parsed.piece)
  if (!piece) return { success: false, error: `piece "${parsed.piece}" is not installed` }

  const actionDef = piece.metadata.actions[parsed.action]
  if (!actionDef) return { success: false, error: `action "${parsed.action}" not found in piece "${parsed.piece}"` }

  const { auth: rawAuth, sessionId, propsValue } = extractPropsAndAuth(args)
  const auth = resolveAuthValue(rawAuth)
  coerceCustomAuthTypes(auth, piece.metadata.auth)
  normalizeCustomApiCall(parsed.action, propsValue)
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

function normalizeCustomApiCall(
  action: string,
  propsValue: Record<string, unknown>,
): void {
  if (action !== 'custom_api_call') return
  const u = propsValue.url
  if (typeof u === 'string') {
    propsValue.url = { url: u }
    return
  }
  if (u && typeof u === 'object') {
    if (!('url' in (u as Record<string, unknown>))) {
      const inner = Object.values(u as Record<string, unknown>).find(v => typeof v === 'string')
      if (typeof inner === 'string') propsValue.url = { url: inner }
    }
    return
  }
  const alt = propsValue.path ?? propsValue.endpoint ?? propsValue.relativeUrl
  if (typeof alt === 'string') propsValue.url = { url: alt }
}

// The daemon stores credentials as strings, so a CHECKBOX field arrives as
// "true"/"false" and a NUMBER as a numeric string. Pieces test these with
// truthy/typed checks (e.g. `auth.useAtlasUrl ? …`), where the string "false"
// is truthy — silently wrong. Coerce each custom-auth field back to its
// declared type so string-stored booleans/numbers behave correctly.
function coerceCustomAuthTypes(
  auth: unknown,
  authDef: import('./types.ts').AuthDef | import('./types.ts').AuthDef[] | undefined,
): void {
  if (!auth || typeof auth !== 'object' || !('props' in auth)) return
  const props = (auth as { props?: Record<string, unknown> }).props
  if (!props || typeof props !== 'object') return

  const defs = Array.isArray(authDef) ? authDef : authDef ? [authDef] : []
  const custom = defs.find(a => a.type === 'CUSTOM_AUTH')
  if (!custom?.props) return

  for (const [key, value] of Object.entries(props)) {
    const type = custom.props[key]?.type
    if (type === 'CHECKBOX') {
      props[key] = value === true || value === 'true'
    } else if (type === 'NUMBER' && typeof value === 'string' && value.trim() !== '') {
      const n = Number(value)
      if (!Number.isNaN(n)) props[key] = n
    }
  }
}
