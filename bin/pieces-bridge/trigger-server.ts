// TriggerServer runs a lightweight HTTP server (separate from MCP stdio) that
// the Digitorn background system calls to poll triggers or manage webhooks.
// Each request body is JSON; each response body is JSON.

import type { PieceLoader } from './loader.ts'
import { resolveAuthValue, makeTriggerContext } from './context.ts'
import type { TriggerPollRequest, TriggerPollResponse, TriggerEnableRequest, TriggerHandshakeRequest } from './types.ts'

export class TriggerServer {
  private server?: ReturnType<typeof Bun.serve>

  constructor(private readonly loader: PieceLoader) {}

  start(port: number): void {
    const loader = this.loader
    this.server = Bun.serve({
      port,
      fetch: (req) => handleRequest(req, loader),
    })
    process.stderr.write(`pieces-bridge: trigger server on :${port}\n`)
  }

  stop(): void {
    this.server?.stop()
  }
}

async function handleRequest(req: Request, loader: PieceLoader): Promise<Response> {
  const url = new URL(req.url)

  // GET endpoints (read-only)
  if (req.method === 'GET') {
    if (url.pathname === '/health') return json({ ok: true })
    if (url.pathname === '/pieces') return json(await handlePiecesList(loader))
    if (url.pathname.startsWith('/pieces/') && url.pathname.endsWith('/auth')) {
      const pieceId = url.pathname.split('/')[2]
      return json(await handlePieceAuth(pieceId, loader))
    }
    if (url.pathname.startsWith('/pieces/') && url.pathname.endsWith('/status')) {
      const pieceId = url.pathname.split('/')[2]
      return json(await handlePieceStatus(pieceId, loader))
    }
    return json({ error: 'not found' }, 404)
  }

  // POST endpoints
  if (req.method !== 'POST') return json({ error: 'method not allowed' }, 405)

  try {
    const body = await req.json()

    if (url.pathname === '/trigger/poll') return json(await handlePoll(body as TriggerPollRequest, loader))
    if (url.pathname === '/trigger/enable') return json(await handleEnable(body as TriggerEnableRequest, loader))
    if (url.pathname === '/trigger/disable') return json(await handleDisable(body as TriggerEnableRequest, loader))
    if (url.pathname === '/trigger/handshake') return json(await handleHandshake(body as TriggerHandshakeRequest, loader))

    return json({ error: 'not found' }, 404)
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err)
    return json({ error: msg }, 500)
  }
}

async function handlePoll(req: TriggerPollRequest, loader: PieceLoader): Promise<TriggerPollResponse> {
  const piece = await loader.getPiece(req.piece)
  if (!piece) throw new Error(`piece "${req.piece}" not installed`)

  const triggerDef = piece.metadata.triggers?.[req.trigger]
  if (!triggerDef) throw new Error(`trigger "${req.trigger}" not found in "${req.piece}"`)

  const auth = resolveAuthValue(req.auth)

  // Restore store state from the request.
  const storeState = new Map<string, unknown>(Object.entries(req.storeState ?? {}))
  const ctx = makeTriggerContext(auth, req.props ?? {}, `poll:${req.piece}:${req.trigger}`, req.trigger, req.webhookUrl ?? '')
  // Override store with persisted state so trigger cursors survive across polls.
  const storeOverride = {
    put: async (k: string, v: unknown) => { storeState.set(k, v) },
    get: async <T>(k: string): Promise<T | null> => (storeState.has(k) ? (storeState.get(k) as T) : null),
    delete: async (k: string) => { storeState.delete(k) },
  }
  ;(ctx as Record<string, unknown>).store = storeOverride

  const events = await triggerDef.run(ctx)
  return {
    events: events ?? [],
    storeState: Object.fromEntries(storeState),
  }
}

async function handleEnable(req: TriggerEnableRequest, loader: PieceLoader): Promise<{ ok: boolean }> {
  const piece = await loader.getPiece(req.piece)
  if (!piece) throw new Error(`piece "${req.piece}" not installed`)

  const triggerDef = piece.metadata.triggers?.[req.trigger]
  if (!triggerDef?.onEnable) return { ok: true }

  const auth = resolveAuthValue(req.auth)
  const storeState = new Map<string, unknown>(Object.entries(req.storeState ?? {}))
  const ctx = makeTriggerContext(auth, req.props ?? {}, `enable:${req.piece}:${req.trigger}`, req.trigger, req.webhookUrl)
  const storeOverride = {
    put: async (k: string, v: unknown) => { storeState.set(k, v) },
    get: async <T>(k: string): Promise<T | null> => (storeState.has(k) ? (storeState.get(k) as T) : null),
    delete: async (k: string) => { storeState.delete(k) },
  }
  ;(ctx as Record<string, unknown>).store = storeOverride

  await triggerDef.onEnable(ctx)
  return { ok: true }
}

async function handleDisable(req: TriggerEnableRequest, loader: PieceLoader): Promise<{ ok: boolean }> {
  const piece = await loader.getPiece(req.piece)
  if (!piece) throw new Error(`piece "${req.piece}" not installed`)

  const triggerDef = piece.metadata.triggers?.[req.trigger]
  if (!triggerDef?.onDisable) return { ok: true }

  const auth = resolveAuthValue(req.auth)
  const storeState = new Map<string, unknown>(Object.entries(req.storeState ?? {}))
  const ctx = makeTriggerContext(auth, req.props ?? {}, `disable:${req.piece}:${req.trigger}`, req.trigger, req.webhookUrl)
  const storeOverride = {
    put: async (k: string, v: unknown) => { storeState.set(k, v) },
    get: async <T>(k: string): Promise<T | null> => (storeState.has(k) ? (storeState.get(k) as T) : null),
    delete: async (k: string) => { storeState.delete(k) },
  }
  ;(ctx as Record<string, unknown>).store = storeOverride

  await triggerDef.onDisable(ctx)
  return { ok: true }
}

async function handleHandshake(req: TriggerHandshakeRequest, loader: PieceLoader): Promise<unknown> {
  const piece = await loader.getPiece(req.piece)
  if (!piece) throw new Error(`piece "${req.piece}" not installed`)

  const triggerDef = piece.metadata.triggers?.[req.trigger]
  if (!triggerDef?.onHandshake) return { status: 200 }

  const auth = resolveAuthValue(req.auth)
  const storeState = new Map<string, unknown>(Object.entries(req.storeState ?? {}))
  const ctx = makeTriggerContext(
    auth,
    req.props ?? {},
    `handshake:${req.piece}:${req.trigger}`,
    req.trigger,
    req.webhookUrl,
    req.payload,
  )
  const storeOverride = {
    put: async (k: string, v: unknown) => { storeState.set(k, v) },
    get: async <T>(k: string): Promise<T | null> => (storeState.has(k) ? (storeState.get(k) as T) : null),
    delete: async (k: string) => { storeState.delete(k) },
  }
  ;(ctx as Record<string, unknown>).store = storeOverride

  return await triggerDef.onHandshake(ctx)
}

function json(data: unknown, status = 200): Response {
  return new Response(JSON.stringify(data), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

// ── Piece metadata endpoints ──────────────────────────────────────────

interface PieceSummary {
  id: string
  displayName: string
  description: string
  logoUrl: string
  authType: string
  authDisplayName: string
  actions: Array<{ name: string; displayName: string; description: string }>
  triggers: Array<{ name: string; displayName: string; description: string; type: string }>
}

async function handlePiecesList(loader: PieceLoader): Promise<{ pieces: PieceSummary[]; count: number }> {
  const pieces = loader.getAllManifests()
  const summaries: PieceSummary[] = pieces.map(p => pieceToSummary(p))
  return { pieces: summaries, count: summaries.length }
}

function pieceToSummary(p: { id: string; displayName: string; description: string; logoUrl: string; metadata: { actions: Record<string, { displayName: string; description?: string; requireAuth: boolean }>; triggers: Record<string, { displayName: string; description?: string; type: string }>; auth?: { type: string; displayName?: string } | Array<{ type: string; displayName?: string }> } }): PieceSummary {
  const auth = p.metadata.auth
  const authType = Array.isArray(auth) ? auth.map(a => a.type).join('+') : (auth?.type ?? 'none')
  const authDisplayName = Array.isArray(auth) ? auth.map(a => a.displayName ?? a.type).join(' / ') : (auth?.displayName ?? '')
  return {
    id: p.id,
    displayName: p.displayName,
    description: p.description,
    logoUrl: p.logoUrl,
    authType,
    authDisplayName,
    actions: Object.entries(p.metadata.actions ?? {}).map(([name, a]) => ({
      name,
      displayName: a.displayName,
      description: a.description ?? '',
    })),
    triggers: Object.entries(p.metadata.triggers ?? {}).map(([name, t]) => ({
      name,
      displayName: t.displayName,
      description: t.description ?? '',
      type: t.type,
    })),
  }
}

async function handlePieceAuth(pieceId: string, loader: PieceLoader): Promise<unknown> {
  const piece = loader.getManifest(pieceId)
  if (!piece) return { error: `piece "${pieceId}" not found` }

  const auth = piece.metadata.auth
  if (!auth) return { type: 'none', fields: [], options: [] }

  // auth can be a single object or an array of auth options
  const authOptions = Array.isArray(auth) ? auth : [auth]

  const options = authOptions.map((opt, idx) => {
    const fields: Array<{ name: string; type: string; displayName: string; description: string; required: boolean; options?: Array<{ label: string; value: string }> }> = []

    switch (opt.type) {
      case 'SECRET_TEXT':
        fields.push({
          name: 'value',
          type: 'string',
          displayName: opt.displayName ?? 'API Key / Token',
          description: opt.description ?? 'Enter your API key or token.',
          required: true,
        })
        break
      case 'BASIC_AUTH':
        fields.push(
          { name: 'username', type: 'string', displayName: 'Username', description: 'Basic auth username.', required: true },
          { name: 'password', type: 'string', displayName: 'Password', description: 'Basic auth password.', required: true },
        )
        break
      case 'OAUTH2':
        // OAuth2 fields are handled by the daemon's OAuth flow
        break
      case 'CUSTOM_AUTH':
        if (opt.props) {
          for (const [key, prop] of Object.entries(opt.props)) {
            const field: { name: string; type: string; displayName: string; description: string; required: boolean; options?: Array<{ label: string; value: string }> } = {
              name: key,
              type: prop.type?.toLowerCase() ?? 'string',
              displayName: prop.displayName ?? key,
              description: prop.description ?? '',
              required: prop.required ?? false,
            }
            // Pass dropdown options for STATIC_DROPDOWN
            if ((prop.type === 'STATIC_DROPDOWN' || prop.type === 'STATIC_MULTI_SELECT_DROPDOWN') && prop.options?.options) {
              field.options = prop.options.options.map((o: { label: string; value: unknown }) => ({
                label: o.label,
                value: String(o.value ?? ''),
              }))
            }
            fields.push(field)
          }
        }
        break
    }

    return {
      id: `option_${idx}`,
      type: opt.type,
      displayName: opt.displayName ?? opt.type,
      description: opt.description ?? '',
      fields,
      oauth: opt.type === 'OAUTH2' ? {
        authUrl: opt.authUrl ?? '',
        tokenUrl: opt.tokenUrl ?? '',
        scope: opt.scope ?? [],
      } : undefined,
    }
  })

  // For single auth, return flat structure; for multiple, return options array
  if (options.length === 1) {
    return { ...options[0], options: undefined }
  }
  return { type: 'multiple', displayName: 'Authentication', description: '', fields: [], options }
}

async function handlePieceStatus(pieceId: string, loader: PieceLoader): Promise<unknown> {
  const piece = loader.getManifest(pieceId)
  if (!piece) return { error: `piece "${pieceId}" not found` }

  return {
    id: piece.id,
    displayName: piece.displayName,
    hasAuth: !!piece.metadata.auth,
    authType: piece.metadata.auth?.type ?? 'none',
    actionCount: Object.keys(piece.metadata.actions ?? {}).length,
    triggerCount: Object.keys(piece.metadata.triggers ?? {}).length,
  }
}
