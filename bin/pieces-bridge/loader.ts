import { readdir } from 'node:fs/promises'
import { join, basename, extname } from 'node:path'
import type { LoadedPiece, PieceInstance, PropDef } from './types.ts'

// Piece bundles live in DIGITORN_PIECES_DIR. Each file is a self-contained
// esbuild bundle: <piece-id>.js (e.g. github.js, google_sheets.js).

export class PieceLoader {
  private pieces = new Map<string, LoadedPiece>()
  private tools: import('@modelcontextprotocol/sdk/types.js').Tool[] = []
  private loaded = false

  constructor(private readonly piecesDir: string) {}

  async load(): Promise<void> {
    this.pieces.clear()
    this.tools = []

    let files: string[]
    try {
      files = await readdir(this.piecesDir)
    } catch {
      return
    }

    const bundles = files.filter(f => extname(f) === '.js')
    await Promise.allSettled(bundles.map(f => this.loadBundle(join(this.piecesDir, f))))
    this.loaded = true
  }

  private async loadBundle(path: string): Promise<void> {
    let mod: Record<string, unknown>
    try {
      mod = await import(path)
    } catch (e) {
      process.stderr.write(`pieces-bridge: failed to load ${path}: ${e}\n`)
      return
    }

    const piece = extractPiece(mod)
    if (!piece) {
      process.stderr.write(`pieces-bridge: no piece found in ${path}\n`)
      return
    }

    let meta: ReturnType<PieceInstance['metadata']>
    try {
      meta = piece.metadata()
    } catch (e) {
      process.stderr.write(`pieces-bridge: metadata() failed for ${path}: ${e}\n`)
      return
    }

    const id = bundleId(basename(path, '.js'))
    const loaded: LoadedPiece = { id, displayName: meta.displayName, description: meta.description ?? '', logoUrl: meta.logoUrl ?? '', instance: piece, metadata: meta, bundlePath: path }
    this.pieces.set(id, loaded)

    for (const [actionName, action] of Object.entries(meta.actions ?? {})) {
      this.tools.push(buildToolSpec(id, actionName, action, meta))
    }
  }

  getTools(): import('@modelcontextprotocol/sdk/types.js').Tool[] {
    return this.tools
  }

  getPiece(id: string): LoadedPiece | undefined {
    return this.pieces.get(id)
  }

  getAllPieces(): LoadedPiece[] {
    return Array.from(this.pieces.values())
  }

  isLoaded(): boolean {
    return this.loaded
  }

  reload(): Promise<void> {
    return this.load()
  }
}

function extractPiece(mod: Record<string, unknown>): PieceInstance | null {
  for (const val of Object.values(mod)) {
    if (val && typeof val === 'object' && typeof (val as PieceInstance).metadata === 'function') {
      return val as PieceInstance
    }
  }
  return null
}

function bundleId(filename: string): string {
  return filename.toLowerCase().replace(/-/g, '_')
}

function buildToolSpec(pieceId: string, actionName: string, action: import('./types.ts').ActionDef, meta: import('./types.ts').PieceMetadata): import('@modelcontextprotocol/sdk/types.js').Tool {
  const toolName = `ap_${pieceId}__${actionName}`
  const schema = buildInputSchema(action.props ?? {}, meta.auth, action.requireAuth)

  return {
    name: toolName,
    description: buildDescription(action, meta),
    inputSchema: schema,
  }
}

function buildDescription(action: import('./types.ts').ActionDef, meta: import('./types.ts').PieceMetadata): string {
  const parts = [`[${meta.displayName}] ${action.displayName}`]
  if (action.description) parts.push(action.description)
  if (action.aiMetadata?.description) parts.push(action.aiMetadata.description)
  return parts.join(' — ')
}

function buildInputSchema(props: Record<string, PropDef>, auth: import('./types.ts').AuthDef | import('./types.ts').AuthDef[] | undefined, requireAuth: boolean): { type: 'object'; properties: Record<string, unknown>; required?: string[] } {
  const properties: Record<string, unknown> = {}
  const required: string[] = []

  if (requireAuth && auth) {
    const authArray = Array.isArray(auth) ? auth : [auth]
    // For multiple auth options, use the first one that has fields; otherwise use first
    const primaryAuth = authArray.find(a => a.type === 'SECRET_TEXT' || a.type === 'CUSTOM_AUTH' || a.type === 'BASIC_AUTH') ?? authArray[0]
    properties['_ap_auth'] = buildAuthSchema(primaryAuth)
    required.push('_ap_auth')
  }

  properties['_ap_session'] = { type: 'string', description: 'Session ID for scoped store (optional).' }

  for (const [key, prop] of Object.entries(props)) {
    if (prop.type === 'MARKDOWN') continue
    properties[key] = propToJsonSchema(prop)
    if (prop.required) required.push(key)
  }

  return { type: 'object', properties, required: required.length > 0 ? required : undefined }
}

function buildAuthSchema(auth: import('./types.ts').AuthDef): unknown {
  switch (auth.type) {
    case 'SECRET_TEXT':
      return {
        type: 'object',
        description: 'API key / token for this connector.',
        properties: {
          type: { type: 'string', enum: ['secret_text'] },
          value: { type: 'string', description: auth.displayName ?? 'API key or token.' },
        },
        required: ['type', 'value'],
      }
    case 'BASIC_AUTH':
      return {
        type: 'object',
        properties: {
          type: { type: 'string', enum: ['basic'] },
          username: { type: 'string' },
          password: { type: 'string' },
        },
        required: ['type', 'username', 'password'],
      }
    case 'OAUTH2':
      return {
        type: 'object',
        properties: {
          type: { type: 'string', enum: ['oauth2'] },
          accessToken: { type: 'string' },
          tokenType: { type: 'string' },
          expiresAt: { type: 'number' },
          refreshToken: { type: 'string' },
          scope: { type: 'string' },
        },
        required: ['type', 'accessToken'],
      }
    case 'CUSTOM_AUTH':
      return buildCustomAuthSchema(auth)
    default:
      return { type: 'object', description: 'Auth credentials for this connector.' }
  }
}

function buildCustomAuthSchema(auth: import('./types.ts').AuthDef): unknown {
  const fieldProps: Record<string, unknown> = {
    type: { type: 'string', enum: ['custom'] },
  }
  for (const [key, prop] of Object.entries(auth.props ?? {})) {
    fieldProps[key] = propToJsonSchema(prop)
  }
  return {
    type: 'object',
    description: auth.description ?? 'Custom authentication credentials.',
    properties: { type: { type: 'string', enum: ['custom'] }, fields: { type: 'object', properties: fieldProps } },
    required: ['type', 'fields'],
  }
}

function propToJsonSchema(prop: PropDef): unknown {
  const base = { description: prop.description ?? prop.displayName }

  switch (prop.type) {
    case 'SHORT_TEXT':
    case 'LONG_TEXT':
    case 'URL':
    case 'DATE_TIME':
      return { type: 'string', ...base }
    case 'SECRET_TEXT':
      return { type: 'string', ...base }
    case 'NUMBER':
      return { type: 'number', ...base }
    case 'CHECKBOX':
      return { type: 'boolean', ...base }
    case 'STATIC_DROPDOWN': {
      const opts = (prop.options as import('./types.ts').StaticOptions | undefined)?.options ?? []
      const enums = opts.map(o => o.value).filter(v => v !== null && v !== undefined)
      return enums.length > 0
        ? { type: 'string', enum: enums, ...base }
        : { type: 'string', ...base }
    }
    case 'STATIC_MULTI_SELECT_DROPDOWN': {
      const opts = (prop.options as import('./types.ts').StaticOptions | undefined)?.options ?? []
      const enums = opts.map(o => o.value).filter(v => v !== null && v !== undefined)
      return enums.length > 0
        ? { type: 'array', items: { type: 'string', enum: enums }, ...base }
        : { type: 'array', items: { type: 'string' }, ...base }
    }
    case 'DROPDOWN':
      return { ...base, description: (base.description ?? '') + ' (pass a string ID or an object, e.g. {owner:"octocat",repo:"hello"} for repository dropdowns)' }
    case 'MULTI_SELECT_DROPDOWN':
      return { type: 'array', items: {}, ...base, description: (base.description ?? '') + ' (array of IDs or objects)' }
    case 'JSON':
    case 'OBJECT':
    case 'DYNAMIC_PROPERTIES':
      return { type: 'object', ...base }
    case 'ARRAY':
      return { type: 'array', items: prop.items ? propToJsonSchema(prop.items) : {}, ...base }
    case 'FILE':
      return { type: 'string', ...base, description: (base.description ?? '') + ' (base64-encoded file content)' }
    default:
      return { type: 'string', ...base }
  }
}
