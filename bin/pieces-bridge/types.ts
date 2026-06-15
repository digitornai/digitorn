// Duck-typed interfaces for Activepieces piece bundles.
// Pieces are self-contained bundles; we don't import AP framework directly.

export interface PieceMetadata {
  displayName: string
  description: string
  logoUrl: string
  authors: string[]
  categories: string[]
  actions: Record<string, ActionDef>
  triggers: Record<string, TriggerDef>
  auth?: AuthDef
  minimumSupportedRelease?: string
}

export interface ActionDef {
  displayName: string
  description: string
  props: Record<string, PropDef>
  requireAuth: boolean
  run(context: ActionContext): Promise<unknown>
  test?(context: ActionContext): Promise<unknown>
  aiMetadata?: { description?: string; idempotent?: boolean }
}

export interface TriggerDef {
  displayName: string
  description: string
  props: Record<string, PropDef>
  requireAuth: boolean
  type: 'WEBHOOK' | 'POLLING' | 'APP_WEBHOOK' | 'EMPTY'
  run(context: TriggerContext): Promise<unknown[]>
  test?(context: TriggerContext): Promise<unknown[]>
  onEnable?(context: TriggerContext): Promise<void>
  onDisable?(context: TriggerContext): Promise<void>
  onHandshake?(context: TriggerContext): Promise<{ status: number; headers?: Record<string, string>; body?: unknown }>
  handshakeConfiguration?: {
    strategy: string
    payloadType?: string
  }
  sampleData?: unknown
}

export interface PropDef {
  displayName: string
  description?: string
  required?: boolean
  type: string
  defaultValue?: unknown
  options?: StaticOptions | DynamicOptions
  items?: PropDef
  properties?: Record<string, PropDef>
}

export interface StaticOptions {
  options: Array<{ label: string; value: unknown }>
  disabled?: boolean
}

export interface DynamicOptions {
  options(ctx: { auth: unknown; propsValue: Record<string, unknown> }): Promise<StaticOptions>
  refreshers?: string[]
}

export interface AuthDef {
  type: string
  displayName?: string
  description?: string
  props?: Record<string, PropDef>
  authUrl?: string
  tokenUrl?: string
  scope?: string[]
}

export interface PieceInstance {
  metadata(): PieceMetadata
}

// Auth injected by the daemon as _ap_auth in tool arguments.
export type APAuth =
  | { type: 'secret_text'; value: string }
  | { type: 'custom'; fields: Record<string, string> }
  | { type: 'oauth2'; accessToken: string; tokenType: string; expiresAt?: number; refreshToken?: string; scope?: string }
  | { type: 'basic'; username: string; password: string }
  | { type: 'none' }

// Minimal ActionContext the bridge provides to pieces.
export interface ActionContext {
  auth: unknown
  propsValue: Record<string, unknown>
  store: StoreContext
  files: FilesContext
  server: ServerContext
  project: ProjectContext
  flows: FlowsContext
  step: StepContext
  connections: ConnectionsContext
  tags: TagsContext
  run: RunContext
  executionType: string
  output: OutputContext
  agent: null
}

export interface TriggerContext extends ActionContext {
  webhookUrl: string
  payload: { body: unknown; headers: Record<string, string>; queryParams: Record<string, string> }
}

export interface StoreContext {
  put(key: string, value: unknown, scope?: string): Promise<void>
  get<T>(key: string, scope?: string): Promise<T | null>
  delete(key: string, scope?: string): Promise<void>
}

export interface FilesContext {
  write(file: { fileName: string; data: Buffer | Uint8Array; contentType: string }): Promise<{
    url: string
    mimeType: string
    extension: string
    size: number
    filename: string
  }>
}

export interface ServerContext {
  apiUrl: string
  publicUrl: string
  token: string
}

export interface ProjectContext {
  id: string
  externalId(): Promise<string>
}

export interface FlowsContext {
  current: { id: string; version: { id: string } }
  list(): Promise<Array<{ id: string; version: { id: string } }>>
}

export interface StepContext {
  name: string
}

export interface ConnectionsContext {
  get(name: string): Promise<unknown>
}

export interface TagsContext {
  add(tag: string): Promise<void>
  addMany(tags: string[]): Promise<void>
}

export interface RunContext {
  id: string
  stop(params?: { response?: unknown }): void
  pause(params?: unknown): Promise<void>
  respond(params?: unknown): Promise<void>
  createWaitpoint(params?: unknown): Promise<{ id: string; response: unknown }>
}

export interface OutputContext {
  update(markdown: string): Promise<void>
}

// Loaded piece entry.
export interface LoadedPiece {
  id: string           // e.g. "github"
  displayName: string
  description: string
  logoUrl: string
  instance: PieceInstance
  metadata: PieceMetadata
  bundlePath: string
}

// Trigger poll request/response (HTTP trigger server).
export interface TriggerPollRequest {
  piece: string
  trigger: string
  auth: APAuth
  props: Record<string, unknown>
  storeState: Record<string, unknown>
  webhookUrl?: string
}

export interface TriggerPollResponse {
  events: unknown[]
  storeState: Record<string, unknown>
}

export interface TriggerEnableRequest {
  piece: string
  trigger: string
  auth: APAuth
  props: Record<string, unknown>
  webhookUrl: string
  storeState?: Record<string, unknown>
}

export interface TriggerHandshakeRequest {
  piece: string
  trigger: string
  auth: APAuth
  props: Record<string, unknown>
  webhookUrl: string
  storeState?: Record<string, unknown>
  payload: { body: unknown; headers: Record<string, string>; queryParams: Record<string, string> }
}
