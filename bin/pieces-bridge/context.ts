import type { ActionContext, TriggerContext, APAuth, StoreContext, FilesContext } from './types.ts'

// resolveAuthValue converts the daemon's _ap_auth wire format to the value
// an Activepieces piece action expects in context.auth.
export function resolveAuthValue(auth: APAuth | undefined): unknown {
  if (!auth || auth.type === 'none') return undefined
  switch (auth.type) {
    case 'secret_text': return auth.value
    case 'basic': return { username: auth.username, password: auth.password }
    // Activepieces CustomAuth exposes its fields to actions as `auth.props.<field>`
    // (the connection value, not the bare fields), so the daemon's flat field map
    // is wrapped under `props` to match what every custom-auth piece reads.
    case 'custom': return { props: auth.fields }
    case 'oauth2': return {
      type: 'OAUTH2',
      access_token: auth.accessToken,
      tokenType: auth.tokenType ?? 'Bearer',
      expiresAt: auth.expiresAt ?? 0,
      refresh_token: auth.refreshToken ?? '',
      scope: auth.scope ?? '',
      data: {},
    }
  }
}

// sessionStores holds in-memory KV stores keyed by session ID.
// Entries expire 1 hour after last access.
const sessionStores = new Map<string, { store: Map<string, unknown>; lastAt: number }>()

function sessionStore(sessionId: string): StoreContext {
  const key = sessionId || 'default'
  let entry = sessionStores.get(key)
  if (!entry) {
    entry = { store: new Map(), lastAt: Date.now() }
    sessionStores.set(key, entry)
  }
  entry.lastAt = Date.now()
  const { store } = entry

  return {
    put: async (k, v) => { store.set(k, v) },
    get: async <T>(k: string): Promise<T | null> => (store.has(k) ? (store.get(k) as T) : null),
    delete: async (k) => { store.delete(k) },
  }
}

// Prune sessions older than 1 hour every 5 minutes.
setInterval(() => {
  const cutoff = Date.now() - 3_600_000
  for (const [k, v] of sessionStores) {
    if (v.lastAt < cutoff) sessionStores.delete(k)
  }
}, 300_000).unref?.()

const bridgeFiles: FilesContext = {
  write: async ({ fileName, data, contentType }) => {
    const buf = data instanceof Uint8Array ? Buffer.from(data) : data as Buffer
    const b64 = buf.toString('base64')
    return {
      url: `data:${contentType};base64,${b64}`,
      mimeType: contentType,
      extension: fileName.split('.').pop() ?? '',
      size: buf.length,
      filename: fileName,
    }
  },
}

const stubConnections = {
  get: async (_name: string): Promise<unknown> => {
    throw new Error('direct connection lookup is not supported in this bridge')
  },
}

// makeActionContext builds the minimal ActionContext for a piece action call.
export function makeActionContext(
  auth: unknown,
  propsValue: Record<string, unknown>,
  sessionId: string,
  stepName: string,
): ActionContext {
  const store = sessionStore(sessionId)
  return {
    auth,
    propsValue,
    store,
    files: bridgeFiles,
    server: { apiUrl: '', publicUrl: '', token: '' },
    project: { id: 'digitorn', externalId: async () => 'digitorn' },
    flows: { current: { id: '', version: { id: '' } }, list: async () => [] },
    step: { name: stepName },
    connections: stubConnections,
    tags: { add: async () => {}, addMany: async () => {} },
    run: {
      id: sessionId || 'bridge',
      stop: () => {},
      pause: async () => {},
      respond: async () => {},
      createWaitpoint: async () => ({ id: '', response: null }),
    },
    executionType: 'BEGIN',
    output: { update: async () => {} },
    agent: null,
  }
}

// makeTriggerContext builds the context for a trigger run/enable/disable call.
export function makeTriggerContext(
  auth: unknown,
  propsValue: Record<string, unknown>,
  sessionId: string,
  stepName: string,
  webhookUrl: string,
  payload?: { body: unknown; headers: Record<string, string>; queryParams: Record<string, string> },
): TriggerContext {
  const base = makeActionContext(auth, propsValue, sessionId, stepName)
  return {
    ...base,
    webhookUrl,
    payload: payload ?? { body: {}, headers: {}, queryParams: {} },
  }
}

// extractPropsAndAuth splits _ap_auth + _ap_session out of raw tool arguments.
export function extractPropsAndAuth(args: Record<string, unknown>): {
  auth: APAuth | undefined
  sessionId: string
  propsValue: Record<string, unknown>
} {
  const { _ap_auth, _ap_session, ...rest } = args
  return {
    auth: _ap_auth as APAuth | undefined,
    sessionId: (_ap_session as string | undefined) ?? '',
    propsValue: rest,
  }
}
