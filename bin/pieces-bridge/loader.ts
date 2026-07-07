import { readdir, readFile } from 'node:fs/promises'
import { join, basename, extname } from 'node:path'
import type { LoadedPiece, PieceInstance, PropDef, PieceMetadata } from './types.ts'

// Piece bundles live in DIGITORN_PIECES_DIR. Each connector is a self-contained
// esbuild bundle <piece-id>.js (e.g. github.js, google_sheets.js), and — when
// present — a sidecar <piece-id>.meta.json carrying its serialisable metadata
// (display info + action/trigger/auth definitions, minus the runnable code).
//
// The loader is LAZY: at startup it reads only the lightweight manifests to
// build the tool list and answer metadata/auth queries. The heavy 20+ MB
// bundle is imported on demand — the first time a connector is actually
// executed or polled for triggers. This keeps memory proportional to the
// connectors in use, not to every bundle on disk, so a catalog of hundreds of
// connectors no longer blows up the process at boot.

// PieceManifest is the serialisable subset of a piece — everything except the
// run/onEnable/… functions, which are only available after a live import.
export interface PieceManifest {
  id: string
  displayName: string
  description: string
  logoUrl: string
  metadata: PieceMetadata
}

export class PieceLoader {
  private manifests = new Map<string, PieceManifest>()
  private bundlePaths = new Map<string, string>()
  private imported = new Map<string, LoadedPiece>()
  private tools: import('@modelcontextprotocol/sdk/types.js').Tool[] = []
  private loaded = false

  constructor(private readonly piecesDir: string) {}

  async load(): Promise<void> {
    this.manifests.clear()
    this.bundlePaths.clear()
    this.imported.clear()
    this.tools = []

    let files: string[]
    try {
      files = await readdir(this.piecesDir)
    } catch {
      return
    }

    const bundles = files.filter(f => extname(f) === '.js')
    await Promise.allSettled(bundles.map(f => this.register(f)))
    this.rebuildTools()
    this.loaded = true
  }

  // register records a bundle's path and loads its manifest WITHOUT importing
  // the bundle. If no sidecar manifest exists it falls back to a one-time
  // import to derive one (keeps connectors built before manifests existed
  // working), which is the only place the eager cost remains.
  private async register(filename: string): Promise<void> {
    const id = bundleId(basename(filename, '.js'))
    this.bundlePaths.set(id, join(this.piecesDir, filename))

    const metaPath = join(this.piecesDir, `${basename(filename, '.js')}.meta.json`)
    try {
      const raw = await readFile(metaPath, 'utf8')
      const m = JSON.parse(raw) as PieceManifest
      m.id = id
      this.manifests.set(id, m)
      return
    } catch {
      // No manifest — derive it by importing once (legacy bundles).
    }

    const piece = await this.importBundle(id)
    if (piece) this.manifests.set(id, toManifest(piece))
    else process.stderr.write(`pieces-bridge: ${filename} has no manifest and failed to import\n`)
  }

  // importBundle loads the heavy bundle and caches the live piece (with its run
  // functions). Idempotent: a second call returns the cached instance.
  private async importBundle(id: string): Promise<LoadedPiece | undefined> {
    const cached = this.imported.get(id)
    if (cached) return cached

    const path = this.bundlePaths.get(id)
    if (!path) return undefined

    let mod: Record<string, unknown>
    try {
      mod = await import(path)
    } catch (e) {
      process.stderr.write(`pieces-bridge: failed to load ${path}: ${e}\n`)
      return undefined
    }

    const piece = extractPiece(mod)
    if (!piece) {
      process.stderr.write(`pieces-bridge: no piece found in ${path}\n`)
      return undefined
    }

    let meta: PieceMetadata
    try {
      meta = piece.metadata()
    } catch (e) {
      process.stderr.write(`pieces-bridge: metadata() failed for ${path}: ${e}\n`)
      return undefined
    }

    const loaded: LoadedPiece = {
      id,
      displayName: meta.displayName,
      description: meta.description ?? '',
      logoUrl: meta.logoUrl ?? '',
      instance: piece,
      metadata: meta,
      bundlePath: path,
    }
    this.imported.set(id, loaded)
    return loaded
  }

  private rebuildTools(): void {
    this.tools = []
    for (const m of this.manifests.values()) {
      for (const [actionName, action] of Object.entries(m.metadata.actions ?? {})) {
        this.tools.push(buildToolSpec(m.id, actionName, action, m.metadata))
      }
    }
  }

  getTools(): import('@modelcontextprotocol/sdk/types.js').Tool[] {
    return this.tools
  }

  // getManifest / getAllManifests serve metadata, auth-schema and listing
  // queries without importing the bundle.
  getManifest(id: string): PieceManifest | undefined {
    return this.manifests.get(id) ?? this.manifests.get(canonicalPieceId(id))
  }

  getAllManifests(): PieceManifest[] {
    return Array.from(this.manifests.values())
  }

  // getPiece imports the bundle on demand — call it only on the execution /
  // trigger paths that need the live run functions.
  async getPiece(id: string): Promise<LoadedPiece | undefined> {
    const cid = canonicalPieceId(id)
    return this.imported.get(id) ?? this.imported.get(cid) ?? this.importBundle(cid)
  }

  isLoaded(): boolean {
    return this.loaded
  }

  reload(): Promise<void> {
    return this.load()
  }
}

// toManifest strips the runnable code from a live piece via a JSON round-trip
// (functions are dropped by JSON.stringify), leaving the serialisable metadata.
function toManifest(p: LoadedPiece): PieceManifest {
  return {
    id: p.id,
    displayName: p.displayName,
    description: p.description,
    logoUrl: p.logoUrl,
    metadata: JSON.parse(JSON.stringify(p.metadata)) as PieceMetadata,
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

// Bundle filenames and piece lookups are canonicalised the same way so a
// connector is addressable by either form (e.g. "telegram-bot" or
// "telegram_bot"): lowercased, hyphens to underscores. The hub catalog, the
// daemon store and the tool names all agree on this canonical id.
export function canonicalPieceId(id: string): string {
  return id.toLowerCase().replace(/-/g, '_')
}

function bundleId(filename: string): string {
  return canonicalPieceId(filename)
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
    properties[key] = propToJsonSchema(prop, key)
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

function propToJsonSchema(prop: PropDef, key?: string): unknown {
  const base = { description: prop.description ?? prop.displayName }

  if (prop.type === 'DYNAMIC_PROPERTIES' && key === 'url') {
    return {
      type: 'object',
      properties: {
        url: { type: 'string', description: 'Relative path (e.g. /user or /repos/{owner}/{repo}) or a full URL.' },
      },
      required: ['url'],
      description: 'Endpoint to call. Pass as an object: { "url": "/path" }.',
    }
  }

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
