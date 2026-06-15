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
  if (req.method !== 'POST') return json({ error: 'method not allowed' }, 405)

  try {
    const body = await req.json()

    if (url.pathname === '/trigger/poll') return json(await handlePoll(body as TriggerPollRequest, loader))
    if (url.pathname === '/trigger/enable') return json(await handleEnable(body as TriggerEnableRequest, loader))
    if (url.pathname === '/trigger/disable') return json(await handleDisable(body as TriggerEnableRequest, loader))
    if (url.pathname === '/trigger/handshake') return json(await handleHandshake(body as TriggerHandshakeRequest, loader))
    if (url.pathname === '/health') return json({ ok: true })

    return json({ error: 'not found' }, 404)
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err)
    return json({ error: msg }, 500)
  }
}

async function handlePoll(req: TriggerPollRequest, loader: PieceLoader): Promise<TriggerPollResponse> {
  const piece = loader.getPiece(req.piece)
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
  const piece = loader.getPiece(req.piece)
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
  const piece = loader.getPiece(req.piece)
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
  const piece = loader.getPiece(req.piece)
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
