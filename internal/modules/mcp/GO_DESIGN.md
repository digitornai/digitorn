# MCP Module — Go Implementation Design

**Status:** Engineer-ready Phase-1 design. Built on PORT_SPEC.md (read in full, 593 lines) + the 4 Go-precedent reports. Locked decisions are treated as settled. Conflicts are flagged `OPEN QUESTION`. No full implementations — design + plan only.

> **DESIGN REVISION (2026-06-07, user-locked): adopt the OFFICIAL Go SDK `modelcontextprotocol/go-sdk` (v1.6.1, Apache/MIT) for the protocol/transport layer.** This SUPERSEDES the §10.1 "hand-roll JSON-RPC" recommendation (which assumed no mature Go SDK — false). Deltas: (a) `jsonrpc.go` is DELETED — use `mcp.NewClient` + `client.Connect(ctx, transport)` → `*mcp.ClientSession` (handshake/version-negotiation/id-correlation are the SDK's job). (b) `transport.go` shrinks to: build `*mcp.CommandTransport{Command: exec.Command(...)}` for stdio (apply `buildSafeEnv` to the `exec.Cmd.Env` at the spawn boundary) and `*mcp.StreamableClientTransport` for http/streamable_http (inject the OAuth `Authorization` header via its HTTP options/middleware — verify exact hook at Chunk 9). (c) `errors.go` maps SDK errors → `MCPTransportError` taxonomy instead of hand-rolling JSON-RPC error parsing; the Python "Session-terminated-404 / exception-group" quirks DON'T apply (those were the Python SDK). (d) `MCPServerEntry` holds a `*mcp.ClientSession`; meta-actions call `session.ListTools/CallTool/ListResources/ReadResource/ListPrompts/GetPrompt/Ping`. (e) **Chunks 2 + 3 collapse** into one "wrap the SDK client (stdio + http)" chunk. EVERYTHING ELSE in this doc stands unchanged — the SDK gives us the wire; the unified guarded pipeline, config schema, gates/index, OAuth token store, sidecar, auto-install are still ours.

**Locked invariants assumed throughout:** WORKER-hosted (mirrors `lsp`); phase-1 = `stdio` + `http`/`streamable_http`; OUTBOUND only; auto-install KEPT but secured (package validation + allow-list + opt-in); OAuth = daemon-resolves-and-injects, worker never persists secrets; ship Python `sdk_fix_wrapper` sidecar; invalid config = HARD compile error; per-call timeout = 3600s ceiling / 120s soft default; `connect` rebuilds the index immediately; add a dynamic `Tools()` RPC.

---

## 1. PACKAGE LAYOUT

Every file to create (NEW) or edit (EDIT). All paths under `C:\Users\ASUS\Documents\digitorn_go`. File set mirrors `internal/modules/lsp/` (`module.go`, `manager.go`, `server.go`, `client.go`, `register.go`) and adds the MCP-specific surfaces.

### 1.1 Worker module — `internal/modules/mcp/`

| File | NEW/EDIT | One-line purpose |
|---|---|---|
| `module.go` | EDIT (stub today) | `Module` impl: `New()`, `Manifest/Init/Start/Stop/Invoke`; registers the 11 meta-action tools; owns the pool + resolver + health loop. Mirrors `lsp/module.go`. |
| `register.go` | NEW (3 lines) | `func init(){ module.MustRegister(func() domainmodule.Module { return New() }) }` — copy `lsp/register.go`, swap ctor. |
| `pool.go` | NEW | `MCPConnectionPool` + `MCPServerEntry` (single mutex serialises connect/disconnect/reconnect; per-server entry; atomic transport swap+rollback). Mirrors `lsp/manager.go` but richer. |
| `transport.go` | NEW | `Transport` interface + `createTransport(type, opts)`; the stdio/http/streamable_http transport types; spawn boundary calls `buildSafeEnv`. Mirrors `lsp/server.go` (spec). |
| `jsonrpc.go` | NEW | Hand-rolled JSON-RPC 2.0 client core: framing, monotonic id, `pending map[any]chan rpcResult`, single reader goroutine, `initialize`→`notifications/initialized` handshake, `_route_send` method routing. Mirrors `lsp/client.go` almost verbatim. |
| `meta.go` | NEW | The 11 meta-actions (`connect`/`disconnect`/`reconnect`/`list_servers`/`list_tools`/`call_tool`/`list_resources`/`read_resource`/`list_prompts`/`get_prompt`/`health_check`) + alias dispatch table. |
| `virtual.go` | NEW | Virtual-tool surface: `mcp_<server>__<tool>` parse regex `^mcp_([^_]+(?:_[^_]+)*)__(.+)$`, build-name round-trip, `LiveTools()` (feeds the `Tools()` RPC), `inputSchema`→`[]tool.ParamSpec` materialization incl. `Path:true` marking + risk-from-name. |
| `pipeline.go` | NEW | **The single unified guarded function** `executeGuarded(ctx, serverID, toolName, args)` — both call paths converge here (§5). Implements §6.4 worker-side steps in order. |
| `result.go` | NEW | Normalized envelope (§2.3): `_source`/`_note` markers, content-block mapping, `result_count`, `status:empty`, 500 KB UTF-8-safe truncation. |
| `cache.go` | NEW | Whitelist-only per-server result cache (`is_cacheable` ⇔ tool ∈ `cacheable_tools`); LRU 200, TTL 300s, key `sha256(json({t,p}))[:16]`; invalidate-on-reconnect/error/refresh/stop. |
| `ratelimit.go` | NEW | Per-server sliding-60s `rate_limit_rpm` limiter (`monotonic` window). |
| `security.go` | NEW | `buildSafeEnv` (allow-list + deny-list + drop `_BLOCKED_ENV_KEYS`), `_INTERNAL_KEYS` strip, sandbox permission gate (`None` vs empty-set), placeholder-id guard, param coercion, OpenAI `format` sanitization. |
| `pathsandbox.go` | NEW | Worker-side recursive nested-path enforcement (`_enforce_path_sandbox`) — defense-in-depth behind the daemon's top-level `EnforceArgs`. |
| `oauth.go` | NEW | Worker-side token **injection** only (header for http/sse, `env_token_var` restart for stdio). NO persistence, NO crypto — receives resolved tokens on the wire. |
| `catalog.go` | NEW | Built-in `CATALOG` (~40 `CatalogEntry`), shorthand→env mapping, `_ARG_APPEND` sentinel, bare-ref resolution (live pool→store→catalog→skip+warn). |
| `registry_resolver.go` | NEW (phase-1.5 stub OK) | MCP registry + Smithery resolvers (resolver order catalog→registry→smithery). Phase-1 ships catalog + smithery; registry browse/search can stub. |
| `autoinstall.go` | NEW | Secured auto-install (§7): package validation `^[a-zA-Z0-9._-]+$` + length cap + allow-list + opt-in flag; no shell. |
| `sdkfix.go` | NEW | Port of `_wrap_with_sdk_fix` *decision* logic (Python-command sniff, `_NON_PYTHON_COMMANDS`, shebang/import sniff); prepends `-m digitorn.modules.mcp.sdk_fix_wrapper`. |
| `config.go` | NEW | Worker-side typed `Init` config decode (reuses `schema.NormalizeServers`); maps `DIGITORN_MODULE_MCP_CONFIG` → live server specs. |
| `errors.go` | NEW | `MCPTransportError{message, code, data, retryable}`; `_is_retryable_error`, `_format_http_error`, exception-group/`errors.Join` unwrap. |
| `module_test.go`, `jsonrpc_test.go`, `pipeline_test.go`, `pool_test.go` | NEW | Unit tests per chunk (§9). |

### 1.2 Python sidecar

| File | NEW/EDIT | Purpose |
|---|---|---|
| `internal/modules/mcp/sidecar/sdk_fix_wrapper.py` | NEW | The tiny Python monkey-patch+`runpy`-exec wrapper (verbatim port of Python). Embedded into the worker binary via `//go:embed` and materialized to a temp path at `Start`, OR shipped alongside the worker binary (see §10 OPEN QUESTION). |

### 1.3 Worker binary wiring

| File | EDIT | Change |
|---|---|---|
| `cmd/digitorn-worker/main.go` | EDIT | Add one side-effect import `_ "github.com/mbathepaul/digitorn/internal/modules/mcp"` (alphabetical, ~line 31-35). |

### 1.4 Worker↔daemon contract — dynamic `Tools()` RPC

| File | EDIT | Change |
|---|---|---|
| `internal/module/service/service.go` | EDIT | Add `Tools` to `Service` interface; `MethodTools = "Tools"`; `toolsHandler` (mirror `manifestsHandler`); entry in `serviceDesc.Methods`. |
| `internal/module/service/types.go` | EDIT | Add `ToolsRequest{ModuleID, AppID, AgentID, UserID}` + `ToolsResponse{Tools []tool.Spec, Generation int64, WorkerID}`. Add `Credentials map[string]string` (or `AuthContext`) field to `InvokeRequest` for daemon-injected OAuth. |
| `internal/module/worker/service.go` | EDIT | Implement `(*moduleService).Tools` — resolve module from bus, call its `LiveTools()`. Re-inject identity into ctx for `Tools` exactly as `Invoke` does. |
| `internal/module/proxy/proxy.go` | EDIT | Add `(*ProxyModule).Tools(ctx)` — live RPC, no cache; populate `InvokeRequest.Credentials` in `Invoke` next to the `AppID/UserID` copy. |
| `internal/domain/module/module.go` | EDIT (optional) | Add a `LiveTooler` interface (`LiveTools(ctx) []tool.Spec`) the worker service type-asserts on; only `mcp` implements it. |

### 1.5 Config schema + compiler validation

| File | EDIT/NEW | Change |
|---|---|---|
| `internal/compiler/schema/enums.go` | EDIT (line 411) | Replace `MCPTransport = {stdio, http, ws}` → `{stdio, sse, streamable_http, http}`. |
| `internal/compiler/schema/mcp.go` | NEW | Typed `McpModuleConfig`, `MCPServerConfig`, `MCPServerSandbox{Permissions, Paths{Read,Write}, AllowedHosts}`, `MCPCacheConfig`, `MCPAuthConfig`; the shared `NormalizeServers(raw any)` (the 3-shape normalizer, ported ONCE). |
| `internal/compiler/validate/mcp.go` | EDIT | Add `CheckMCPConfig` (HARD validation) next to existing `CheckMCPRefs`; reuse `declaredMCPServers`/`mcpKeys`/`suggest.Closest`. |
| `internal/compiler/compiler.go` | EDIT (~line 155) | Wire `validate.CheckMCPConfig(...)` after `CheckMCPRefs`. |
| `internal/compiler/catalog/constraints.go` | EDIT | Add `"allowed_servers": module.ConstraintStringList` to `builtinConstraints`. |

### 1.6 Daemon-side index / gate hooks

| File | EDIT/NEW | Change |
|---|---|---|
| `internal/server/mcp_catalog.go` | NEW | `mcpCatalog` — live `appID → []policy.AvailableAction` map, mutexed; populated from `ProxyModule.Tools()`; re-polled on connect/disconnect/reconnect. |
| `internal/server/registry_actions.go` | EDIT | `registryActions.ForApp`: concat static manifests with `mcpCatalog`; admit `mcp_*` module ids past the declared-modules filter when base `mcp` is declared. `registryToolSpecs.LookupToolSpec`: route `mcp_*` lookups to `mcpCatalog` (else gates fail closed). |
| `internal/server/bootstrap.go` | EDIT | Construct `mcpCatalog`, inject it into `registryActions{}` and `registryToolSpecs{}`; wire the post-connect index-rebuild hook (calls `wiring.Builder.Invalidate(appID,"","")`). |
| `internal/runtime/dispatch/busadapter.go` | EDIT (only if recommendation (a) below is NOT taken) | Optional alias `mcp_<server>.<tool>` → `mcp.call_tool`. **Recommend NOT editing** — worker owns decomposition (see §3). |

### 1.7 Daemon-side OAuth token store

| File | NEW | Purpose |
|---|---|---|
| `internal/server/mcp_oauth/store.go` | NEW | Encrypted token store keyed `(UserID, provider)`; AES-GCM at rest; key file `~/.digitorn/server.key` 0600 atomic-exclusive. |
| `internal/server/mcp_oauth/flow.go` | NEW | PKCE S256, CSRF state (single-use, 10-min TTL, persisted), exchange/refresh, the 5-provider table, the 8 bug-fixes. |
| `internal/server/mcp_oauth/routes.go` | NEW | Hosted HTTP routes (`/oauth/authorize`, `/oauth/callback`, token inject/revoke) — with `client_secret` no longer leaked, DELETE scoped to `(user_id,provider)`. |
| `internal/server/mcp_oauth/resolver.go` | NEW | The daemon-side resolver `ProxyModule.Invoke` calls before each MCP tool dispatch to fill `InvokeRequest.Credentials`. |

---

## 2. WORKER MODULE STRUCTURE

### 2.1 The `Module` impl (`module.go`)

Mirror `lsp/module.go`. `New() *Module` embeds `module.Base` (`pkg/module/base.go`) with `ID:"mcp", Version:"1.0.0"`.

- **`Manifest()`** — returns the Base manifest carrying ONLY the 11 meta-action `tool.Spec`s (static). Virtual tools are NOT in the manifest (they go via `LiveTools()`/`Tools()` RPC). Mark `connect`/`reconnect`/`disconnect` etc. with their risk levels; the LLM-grantable subset is `call_tool`/`list_tools`/`list_servers`.
- **`Init(ctx, cfg map[string]any)`** — decode `cfg` via `mcpconfig.Decode` (reuses `schema.NormalizeServers`), build the `MCPConnectionPool`, resolver, cache, rate-limiters, sandbox table from server config. **No network here** — only build state. Invalid config already rejected at compile; a residual decode error is FATAL (worker refuses to serve — matches `runner.go` loud-fail).
- **`Start(ctx)`** — auto-connect declared servers (PORT_SPEC §1.3 Activation): for each server, resolve → create transport → handshake → `_refresh_capabilities`. **A failing server logs a warning and is stored with `status="error"`; others proceed; the agent still starts.** Start the health loop goroutine. Materialize the sidecar (`sdkfix.go`) to a temp path. Connections live on a **process-lifetime `baseCtx`**, NOT the per-call ctx (the LSP `manager.go:111` lesson — `exec.CommandContext` on the call ctx would kill the server when the first call ends).
- **`Stop(ctx)`** — parallel time-bounded teardown (~10s budget, PORT_SPEC §1.3 / §1.4 §5): cancel `baseCtx`, disconnect all (terminate stdio subprocesses via SIGTERM→hardKill, close http sessions), invalidate caches. Mirror `lsp/module.go:113 stopAll`.
- **`Invoke(ctx, toolName, params)`** — the regex split (PORT_SPEC §10.1 `execute()`): if `toolName` matches `^mcp_…__…$` → `virtual.go` parse → `pipeline.executeGuarded`. Else → `meta.go` dispatch (alias-resolved). **Both converge in `pipeline.executeGuarded` for the actual external call.**
- **`LiveTools(ctx) []tool.Spec`** — iterate the live pool, materialize per-server virtual-tool specs (`virtual.go`). Consumed by the worker's `Tools()` RPC handler. Implements the `LiveTooler` interface.

### 2.2 Connection pool + entries (`pool.go`)

`MCPConnectionPool`:
- One `sync.Mutex` (`lock`) serialises `connect`/`disconnect`/`reconnect`/`disconnectAll` (Python's single `asyncio.Lock`). Tool calls/lists run **concurrently** across servers — they do NOT take `lock` (read a snapshot of the entry under a brief RLock, then release). **Fix the Python hazard:** expose a single locked pool API; never mutate the entry map outside it.
- `MCPServerEntry`: `ServerID, TransportType string, Transport Transport, Tools []MCPToolDef, Resources, Prompts []…, Status string (connected|error|disconnected), AuthConfig *MCPAuthConfig, CreatedAt, LastPing time.Time, Err error, ConnectKwargs ConnectConfig, ToolExamples map[string][]map[string]any, ConsecutiveFailures int`. `ConnectKwargs` stores the **original** config for reconnect.
- **connect:** existing id → `disconnect` first; on `MCPTransportError` set `Status="error"` and **store the failed entry anyway** (listable), re-raise.
- **reconnect:** `maxRetries` from config (`max_reconnect_attempts`, default 4 → 5 tries); backoff `min(base*2^(n-1), 30s)` + 20% jitter (≈ 0,1,2,4,8s). **Atomic transport swap with rollback:** connect new transport → refresh caps → only then commit `entry.Transport`; on refresh failure restore old, close new, count attempt failed. (Avoids half-connected entries.)
- `getConnected(id)`: missing OR `Status!="connected"` OR `!transport.Connected()` → `MCPTransportError`.

### 2.3 Transports (`transport.go`)

```
type Transport interface {
    Send(ctx context.Context, method string, params any) (json.RawMessage, error)
    Connected() bool
    Close(ctx context.Context) error
}
```
`createTransport(transportType string, opts TransportOpts) (Transport, error)`:
- `stdio` (requires `command`) — subprocess; newline-delimited JSON-RPC over stdin/stdout (PORT_SPEC §4.2). Built on `jsonrpc.go`. Spawn calls `security.buildSafeEnv` + `sdkfix.wrap`. Started on `baseCtx`.
- `streamable_http` / `http` (alias; requires `url`) — HTTP POST + streaming, all 4 net/http client timeouts = `timeout`. A `sessionLock` snapshots the session ref then releases (concurrent in-flight allowed, PORT_SPEC §4.6).
- `sse` — phase-1.5 (deferrable). `Unknown transport type` error otherwise.

### 2.4 JSON-RPC layer (`jsonrpc.go`) — the hand-rolled core

This is the LSP `client.go` template (PORT_SPEC §4.1 says no Go SDK; §4.6 lists exactly these requirements):
- **Framing:** stdio = newline-delimited JSON (NOT LSP's `Content-Length`; MCP stdio uses NDJSON); http = one JSON body per POST.
- **Handshake:** send `initialize{protocolVersion:"2024-11-05", capabilities:{}, clientInfo:{name:"digitorn",version:"1.0.0"}}` → store `serverInfo`+`capabilities` → send notification `notifications/initialized` (no id/params) **before any other request** (PORT_SPEC §4.3).
- **Id-correlation:** per-connection monotonic `nextID atomic.Int64`, `pending map[any]chan rpcResult` (id may be int|str), **single reader goroutine** demuxing responses vs notifications by `id`. `safego.Run` panic shield per message (LSP pattern).
- **Method routing (`_route_send`, PORT_SPEC §4.4):** `tools/list`, `tools/call{name,arguments}`, `resources/list`, `resources/read{uri}`, `prompts/list`, `prompts/get{name,arguments}`, `ping`, `initialize`; else log `mcp_unknown_method`, return `{}`. Preserve camelCase aliases on parse: `inputSchema`, `_meta`, `mimeType`, `isError`.
- **Cancellation:** honor ctx; optionally send `notifications/cancelled` on ctx cancel (PORT_SPEC §4.6 — cleaner than Python).

### 2.5 Health loop

A goroutine (`health_interval`, 60s default, range 10–600) pings each connected server (`ping`, 10s timeout): success → `LastPing`, `Status="connected"`; failure → `Status="error"` then trigger `reconnect` with backoff. The loop lives in the module/store (PORT_SPEC §4.5), not the pool. Per-server circuit breaker: after `maxRetries` exhausted, leave `Status="error"` until manual `reconnect` (PORT_SPEC §8 item 22).

---

## 3. WORKER-DAEMON CONTRACT

### 3.1 The dynamic `Tools()` RPC (proto change + regen)

**There is NO `.proto`** (confirmed: `service.go:42-53` is a hand-written `grpc.ServiceDesc` + JSON codec `codec.go` `CodecName="json+module"`). **"Regen mechanism" = a 4-line Go edit, no protoc.** Steps:

1. `service.go`: add `Tools(ctx, *ToolsRequest) (*ToolsResponse, error)` to `Service`; `const MethodTools = "Tools"`; a `toolsHandler` (copy `manifestsHandler` lines 69-81); `{MethodName: MethodTools, Handler: toolsHandler}` in `serviceDesc.Methods`. Additive — old daemon ↔ new worker simply never calls `Tools` (NOT_FOUND only if mismatched, never happens since both ship together).
2. `types.go`: `ToolsRequest{ModuleID string, AppID, AgentID, UserID string}` (identity-scoped — virtual tools are policy-filtered per agent, PORT_SPEC §3.2). `ToolsResponse{Tools []tool.Spec, Generation int64, WorkerID string}`. `Generation` lets the daemon detect staleness and skip a needless index rebuild.
3. `worker/service.go`: implement `Tools` — re-inject identity into ctx (mirror `Invoke` `tool.WithIdentity`), resolve module via `bus.Get(req.ModuleID)`, type-assert `LiveTooler`, call `LiveTools(ctx)`. Non-MCP modules return their static `Manifest().Tools`.
4. `proxy.go`: `(*ProxyModule).Tools(ctx)` — `picker.Pick`, raw `conn.GRPC().Invoke(ctx, "/"+ServiceName+"/"+MethodTools, ...)` (mirror the `Manifests` fetch at `proxy.go:100-107`). **No caching** (unlike `Manifest()`).

### 3.2 How the daemon rebuilds the index from `Tools()` on connect/disconnect

- A new daemon struct `mcpCatalog` (`internal/server/mcp_catalog.go`): `map[appID][]policy.AvailableAction` + mutex + per-app `Generation`.
- **Trigger:** after a `connect`/`reconnect`/`disconnect` meta-action `Invoke` returns, the worker sets a result-metadata flag `tools_changed:true` (+ new `Generation`). The daemon-side path (proxy or a thin engine hook) observes it, calls `ProxyModule.Tools(ctx)`, rebuilds `mcpCatalog[appID]`, then `wiring.Builder.Invalidate(appID, "", "")` (`builder.go:370`) so the **next turn** rebuilds the per-agent index with the new virtual tools. This is the locked "connect rebuilds the index immediately" decision; the invalidation primitive already exists.
- `mcpCatalog.ForApp(appID)` feeds `registryActions.ForApp` (concat with static manifests). The naming convention: index/policy/dispatch key = **single-dot** `mcp_<server>.<tool>`; the `__` form is wire-only (`Sanitize`/`Canonicalize` already handle the round-trip; no `toolname`/`busadapter`/`engine` changes needed).

### 3.3 How `Invoke` routes `mcp_server__tool` vs the 11 meta-actions

**Recommendation (a) — worker owns decomposition** (matches WORKER-hosted decision, keeps daemon FQN-clean):
- The `mcp` `ProxyModule` claims the `mcp_<server>` namespace at the dispatch terminal. `BusAdapter.Dispatch` splits `module="mcp_slack"`, `action="post_message"`, calls `Bus.Call(ctx,"mcp_slack","post_message",raw)`. Register a routing shim so any `mcp_*` module id reaches the single `mcp` proxy; the worker `Invoke` receives `ToolName="post_message"` + `ModuleID="mcp_slack"`, re-derives `(server_id="slack", tool="post_message")`, and funnels into `pipeline.executeGuarded`.
- **`OPEN QUESTION`:** the servicebus has only an `mcp` module, not `mcp_slack`. Cleanest is to have the worker `Invoke` accept the FULL virtual action name (`mcp_slack__post_message`) as `ToolName` with `ModuleID="mcp"`, and parse it with `^mcp_([^_]+(?:_[^_]+)*)__(.+)$`. This requires the daemon dispatch to send `ModuleID="mcp", ToolName="mcp_slack__post_message"` for virtual tools — i.e. the daemon-side alias `mcp_slack.post_message` → `mcp` module + `mcp_slack__post_message` tool. **This is the one routing decision that needs a daemon-architecture call** (precedent: `aliasLegacyToolModule` at `busadapter.go:103-108` already rewrites `workspace.read`→`filesystem` per-tool). **Recommend:** daemon rewrites `mcp_<server>.<tool>` → `(ModuleID="mcp", ToolName="mcp_<server>__<tool>")` at dispatch; worker re-parses. This is minimal and keeps ALL MCP logic worker-side.
- The 11 meta-actions arrive as `ModuleID="mcp", ToolName="connect"|"call_tool"|…`; `meta.go` resolves aliases and dispatches. Meta `call_tool{server_id, tool_name, arguments}` ALSO funnels into `pipeline.executeGuarded` (the §1.4 convergence).

### 3.4 Concurrency, deadlines

- `worker/service.go.Invoke`/`Tools` are stateless on the call path; gRPC dispatches each RPC on its own goroutine; the manager runs `connPoolSize=8` HTTP/2 conns per worker (`manager.go:261`). MCP module internal state (pool, per-user pools, cache, rate-limiters) MUST be concurrency-safe — its own mutexes.
- **Deadline:** `WorkerPool.InvokeTimeout = 3600s` (ceiling, `config.go:157`) → `proxy.invokeTimeout` → hard gRPC ctx deadline. `DeadlineMs` is advisory. The **120s soft default** is a worker-side policy in `pipeline.go` (read `tool_call_timeout` per server; default soft 120s, ceiling 3600s). Resolves OPEN QUESTION #1.
- Error split: transport/protocol failure → Go error from `Invoke` (retryable). Tool failure (`isError`) / guard rejection / `BudgetExceeded`/`CircuitOpen`/`Timeout` → `tool.Result{Success:false}`, NO Go error (no transport retry). Matches `worker/service.go:66-78`.

---

## 4. CONFIG SCHEMA DIFF

### 4.1 Fix the transport enum — `internal/compiler/schema/enums.go:411`

```
type MCPTransport string
const (
    MCPTransportStdio          MCPTransport = "stdio"
    MCPTransportSSE            MCPTransport = "sse"
    MCPTransportStreamableHTTP MCPTransport = "streamable_http"
    MCPTransportHTTP           MCPTransport = "http" // alias of streamable_http
)
var AllMCPTransports = []MCPTransport{MCPTransportStdio, MCPTransportSSE, MCPTransportStreamableHTTP, MCPTransportHTTP}
```
Drop `ws` (PORT_SPEC §7.6). The only typed consumer is `CredentialProviderConfig.Transport` (`credentials_schema.go:19`) and the validator at `validate/enums.go:84` — both correct for free. Normalize `http→streamable_http` at parse/connect, not in the enum.

### 4.2 New typed structs — `internal/compiler/schema/mcp.go` (NEW)

```
type McpModuleConfig struct {   // extra:forbid — hand-enforced in CheckMCPConfig
    Workspace  string                      `yaml:"workspace,omitempty"` // daemon-injected, accept-and-ignore
    Servers    any                         `yaml:"servers,omitempty"`   // 3 shapes; normalized once
    Cache      MCPCacheConfig              `yaml:"cache,omitempty"`
    Middleware []map[string]any            `yaml:"middleware,omitempty"`
}
type MCPServerConfig struct {   // server entries are extra:ALLOW (catalog shorthands)
    Transport      MCPTransport            `yaml:"transport,omitempty"`
    Command        string                  `yaml:"command,omitempty"`
    Args           []string                `yaml:"args,omitempty"`
    Env            map[string]string       `yaml:"env,omitempty"`
    URL            string                  `yaml:"url,omitempty"`
    Headers        map[string]string       `yaml:"headers,omitempty"`
    Timeout        float64                 `yaml:"timeout,omitempty"`
    BufferSize     int                     `yaml:"buffer_size,omitempty"` // accepted, unused
    Auth           *MCPAuthConfig          `yaml:"auth,omitempty"`
    Examples       map[string]any          `yaml:"examples,omitempty"`
    RateLimitRPM   int                     `yaml:"rate_limit_rpm,omitempty"`
    Via            string                  `yaml:"via,omitempty"`         // only "smithery"
    SmitheryKey    string                  `yaml:"smithery_key,omitempty"`
    SmitheryNS     string                  `yaml:"smithery_namespace,omitempty"`
    SmitherySlug   string                  `yaml:"smithery_slug,omitempty"`
    Sandbox        *MCPServerSandbox       `yaml:"sandbox,omitempty"`     // none ⇒ blocked (inline)
    Middleware     []map[string]any        `yaml:"middleware,omitempty"`
    CacheTTL       float64                 `yaml:"cache_ttl,omitempty"`
    CacheableTools []string                `yaml:"cacheable_tools,omitempty"`
    Extra          map[string]any          `yaml:",inline"`              // catalog shorthands (token,api_key,…)
}
type MCPServerSandbox struct {  // extra:forbid — hand-enforced
    Permissions  []string         `yaml:"permissions,omitempty"`
    Paths        MCPSandboxPaths  `yaml:"paths,omitempty"`
    AllowedHosts []string         `yaml:"allowed_hosts,omitempty"`
}
type MCPSandboxPaths struct { Read []string `yaml:"read,omitempty"`; Write []string `yaml:"write,omitempty"` }
type MCPCacheConfig struct { TTL int `yaml:"ttl,omitempty"`; MaxSize int `yaml:"max_size,omitempty"`; Enabled *bool `yaml:"enabled,omitempty"` }
type MCPAuthConfig struct {
    Type            string            `yaml:"type,omitempty"`       // only "oauth2" honored
    Provider        string            `yaml:"provider,omitempty"`   // default custom
    ClientID        string            `yaml:"client_id,omitempty"`
    ClientSecret    string            `yaml:"client_secret,omitempty"`
    Scopes          []string          `yaml:"scopes,omitempty"`
    RedirectURI     string            `yaml:"redirect_uri,omitempty"`
    AuthorizeURL    string            `yaml:"authorize_url,omitempty"`
    TokenURL        string            `yaml:"token_url,omitempty"`
    RevokeURL       string            `yaml:"revoke_url,omitempty"` // parsed, unused
    PKCE            *bool             `yaml:"pkce,omitempty"`       // default true
    TokenAuthMethod string            `yaml:"token_auth_method,omitempty"` // default body
    ExtraParams     map[string]any    `yaml:"extra_params,omitempty"`      // → extra_authorize_params
    EnvTokenVar     string            `yaml:"env_token_var,omitempty"`
}
```

**Critical mechanism (from schema-extension report):** `extra:forbid` CANNOT be a struct tag here. `ModuleBlock` is in `permissiveStructs` (`parse/walk.go:18`) and `Config map[string]any` stops the walker (`walk.go:71-74`). So **keep `ModuleBlock.Config` as `map[string]any`**; the typed structs live in `schema` and are decoded ONLY inside `CheckMCPConfig` and inside the worker's `Init`. `extra:forbid` is hand-enforced in `CheckMCPConfig`.

### 4.3 The 3-shape normalization — ported ONCE

`schema.NormalizeServers(raw any) (map[string]MCPServerConfig, []normErr)` in `mcp.go`. Handles:
1. `[]any` of **strings** (`- github`) → `{github: DEFAULT_BARE}`.
2. `map[*]any` with empty/non-dict value (`notion: {}`) → `DEFAULT_BARE`.
3. dict value (inline) → decoded verbatim into `MCPServerConfig`.

`DEFAULT_BARE = MCPServerConfig{Sandbox:&{Permissions:["process.exec","net.http"]}}`. **Both** the compiler validator AND the worker `Init` call this SAME function (Python duplicated it — port once). Note the existing `declaredMCPServers` (`validate/mcp.go:40`) `[]any` branch reads `m["id"]` from a map — **bug for the rewrite**; `NormalizeServers` must handle bare strings.

### 4.4 HARD validation — extend `internal/compiler/validate/mcp.go`

Add `CheckMCPConfig(file, doc, def, bag)`, wired at `compiler.go:155+1`. For `def.Tools.Modules["mcp"]`:
1. `schema.NormalizeServers(block.Config["servers"])`.
2. Per `(id, server)`:
   - **server_id** `^[a-z][a-z0-9_]*$` → `CodeBadRegex` (DGT-E0105). (Locked: HARD error, not Python silent-skip. Resolves OPEN QUESTION #7.)
   - **transport** ∈ `AllMCPTransports` → `(*validator).enum` style `CodeBadEnum` (DGT-E0104) + `suggest.Closest`.
   - **required:** `stdio`⇒`command`; http-family⇒`url` → `CodeMissingRequired` (DGT-E0100).
   - **sandbox deny-by-default:** inline server (has transport/command/url) with no `sandbox:` → HARD error (PORT_SPEC §6.3, §7.2). Bare/empty refs auto-`DEFAULT_BARE`, pass.
   - **sandbox `extra:forbid`** (only `{permissions,paths,allowed_hosts}`) + **permission vocab** `process.*|net.*|fs.*` → `CodeUnknownField`/`CodeBadEnum`.
   - **top-level `extra:forbid`** (`McpModuleConfig`: only `{workspace,servers,cache,middleware}`) → `CodeUnknownField` (DGT-E0101).
   - **timeout** ∈ `[1,300]` → `CodeOutOfRange` (DGT-E0103).
   - **tolerate literal `{{workdir}}`** in sandbox paths (daemon-runtime-injected — don't hard-fail on unresolved templates; `{{secret.}}`/`{{env.}}` already resolved at `compiler.go:135`).
3. **allowed_servers cross-ref:** every id in `constraints.allowed_servers` declared → `CodeUnknownMCPServer` (DGT-E0312) + `suggest.Closest`.
4. **Reject inbound-MCP YAML** (PORT_SPEC §9, §8 item 36) → `CodeUnknownField` on any `app-as-mcp-server` block.

Also: `catalog/constraints.go` add `"allowed_servers": module.ConstraintStringList` (today silently ignored at `constraints.go:19`).

---

## 5. THE UNIFIED GUARDED PIPELINE

**One function, `pipeline.executeGuarded(ctx, serverID, toolName, args)` (worker-side, `pipeline.go`).** Both the virtual-tool path AND the meta `call_tool` path call it — this is the PORT_SPEC §1.4 convergence that fixes the Python divergence (meta path skipped sandbox/rate-limit/cache; virtual path skipped `allowed_servers`).

But the §6.4 controls split across the process boundary. The boundary is drawn by *where each control physically can run*:

| §6.4 step | Control | Owner | Why / where |
|---|---|---|---|
| — | **gate/approval (5-gate)** | **DAEMON** | `Engine.enforceGate` (`engine.go:2565`) + `GateSubTool` (`engine.go:2684`). Module id `mcp_<server>`. No new gate code — gates 0-6 are module-agnostic. |
| — | **risk-from-name → policy** | **DAEMON** | `Gate2Risk` reads `pc.ToolSpec.RiskLevel`; the MCP-aware `LookupToolSpec`+`ForApp` must bake risk-from-name into `Spec.RiskLevel`. Inference itself runs at materialization (`virtual.go`, daemon reads it via `Tools()`). |
| — | **index (BLOCK→drop, APPROVE→gate)** | **DAEMON** | `BuildAgentToolset` excludes denied; `awaitApproval`+`approval.Registry`. |
| — | **`allowed_servers`** | **DAEMON** | Capabilities/constraints layer — enforced for BOTH virtual and meta paths uniformly at the single chokepoint. This is the convergence point that fixes the virtual-path-skips-`allowed_servers` bug. |
| 9 (top-level) | **path-policy on path args** | **DAEMON** | `workdir.EnforceArgs` at `engine.go:2614`, keyed on `Spec.PathParamNames()`. Runs BEFORE the worker round-trip; confined absolute path crosses the wire. |
| 1 | **sandbox gate (`None`→reject; empty-set→allow)** | **WORKER** | At the transport spawn boundary, which only exists worker-side (PORT_SPEC §6.2 "apply at spawn, not config parse"). `security.go`. |
| 2 | **per-server `rate_limit_rpm`** | **WORKER** | Per-server runtime state (`ratelimit.go`). Distinct from the app-level gate-6 `RateLimiter` (which is daemon-side). |
| 3 | **whitelist-only cache lookup** | **WORKER** | Depends on per-server `cacheable_tools` + pool state (`cache.go`). |
| 4 | **strip `_INTERNAL_KEYS`** | **WORKER** | `{_approved,_agent_id,_turn_id,_request_id}` stripped right before the JSON-RPC send (`security.go`). |
| 5 | **OAuth ensure / inject** | **WORKER (inject) + DAEMON (resolve)** | Daemon resolves+injects the token into `InvokeRequest.Credentials`; worker injects into header/`env_token_var` and may swap to per-user pool. Worker NEVER persists (§6). |
| 6 | **placeholder-id guard** | **WORKER** | Needs the per-server `input_schema` (`security.go`). |
| 7 | **required-params validation** | **WORKER** | Against `input_schema.required` (`security.go`). |
| 8 | **param coercion** | **WORKER** | Schema-driven `_coerce_params` (`security.go`). |
| 9 (nested) | **nested path sandbox** | **WORKER** | Recursive string-leaf walk (`pathsandbox.go`) — defense-in-depth behind the daemon's top-level `EnforceArgs`. PORT_SPEC §6.4 step 9 walks ALL leaves; `EnforceArgs` only handles top-level keys. |
| 10 | **env-sandbox at spawn** | **WORKER** | `buildSafeEnv` inside the spawn (`security.go`). |
| 6.6 | **`_source`/`_note` + 500 KB cap** | **WORKER** | Markers wrap raw external output before it crosses the gRPC boundary (`result.go`). Daemon's `partsFromResult` passes through verbatim. |
| 10 | **execute + reconnect (2 attempts, soft/ceiling timeout)** | **WORKER** | `_raw_mcp_call_with_reconnect`; catch `Budget/Circuit/Timeout` → `success=false`. |

**Convergence summary:** there are TWO "one guarded function" layers that cannot merge across the process boundary — the **daemon chokepoint** (governance: gate/approval/risk/`allowed_servers`/top-level-path) and the **worker `executeGuarded`** (transport hygiene: sandbox/rate-limit/cache/strip/coerce/nested-path/markers/cap). Both the virtual-dispatch path and the meta-`call_tool` path funnel into the SAME worker `executeGuarded`, satisfying "every external call gets every control."

---

## 6. OAUTH (daemon-resolves)

### 6.1 Token store location — daemon-side

`internal/server/mcp_oauth/store.go`. DB-backed `UserOAuthToken` keyed `(UserID, provider)` UNIQUE (NOT server_id — two servers sharing a provider share one token, PORT_SPEC §5.5). Columns: `access_token_enc`, `refresh_token_enc` (both encrypted), `token_type`, `expires_at`, `scope`. **The worker never holds the store** (locked decision).

### 6.2 Encrypt-at-rest

Python uses Fernet (AES-128-CBC+HMAC). **Go: AES-GCM** (or `nacl/secretbox`). Encrypt **both** access AND refresh tokens. Key file `~/.digitorn/server.key`, dir `0o700`, key `0o600`, atomic `O_EXCL` create (TOCTOU-safe). **Never log decrypted tokens; truncate `client_id[:12]` in any audit (PORT_SPEC §6.9 item 12).**

### 6.3 Injection into the worker connect/call config

The cross-boundary mechanism: the daemon-side `mcp_oauth/resolver.go` runs inside `ProxyModule.Invoke` (next to the `AppID/UserID` copy at `proxy.go:184`). For an MCP virtual-tool/`call_tool` whose server has `auth_config`, it: resolves `UserID` from ctx → `refreshTokenIfNeeded(buffer=300s)` → fills `InvokeRequest.Credentials` (NEW field, §3.1) with `{token, token_type, url?}`. The worker `oauth.go` reads `Credentials`, injects into the transport header (http/sse) or `env_token_var` (stdio restart), and **discards** after the call. Per-user stdio isolation: worker `_get_or_create_user_pool(UserID)`, LRU `_MAX_USER_POOLS=20`, reconnect on token change.

### 6.4 Per-user isolation across the boundary

`UserID` already rides `InvokeRequest` (`types.go:64`) and the new `ToolsRequest`. The worker keys per-user pools by `UserID`. The daemon keys the token store by `(UserID, provider)`. Coherent end-to-end.

### 6.5 The needs_auth surfacing

When the daemon resolver finds no/expired-unrefreshable token, it returns the canonical blocking shape WITHOUT calling the worker: `tool.Result{Success:false, Metadata:{requires_oauth:true, provider, auth_url, state, server_id}}` (PORT_SPEC §5.7 Branch B, §8 item 16). Built daemon-side in `mcp_oauth/flow.go`.

### 6.6 The 8 bug-fixes applied (PORT_SPEC §5.10)

1. `/pending-oauth` NEVER returns `client_secret`/client_id/redirect_uri (`routes.go`).
2. DELETE token scoped to `(UserID, provider)` (`store.go`), not all-users.
3. session-fallback reads the `user_id` **key**, not `.id` (`resolver.go`).
4. refresh content-type mirrors exchange (basic providers use form on BOTH) (`flow.go`).
5. PKCE `is True` merge guard made explicit — YAML precedence (`flow.go`, mirrors PORT_SPEC §5.3 merge subtleties).
6. `find_token_by_provider` org-wide sharing made opt-in, not silent default (`store.go`).
7. CSRF `state` persisted (DB), 10-min TTL, single-use `pop` (`flow.go`) — multi-worker-safe, not per-process in-memory.
8. add `nonce` + token revocation (`revoke_url` wired) — phase-2 hardening, but the schema field exists now.

PKCE S256: `verifier = token_urlsafe(64)[:128]`; `challenge = base64.RawURLEncoding(sha256(verifier))`. 5-provider table (google/github/slack/microsoft/notion) + custom. Lazy refresh `now >= expires_at - 300`; on failure return nil (force re-auth, never serve stale).

---

## 7. AUTO-INSTALL (kept+secured)

**Where it runs:** WORKER (`autoinstall.go`), inside `connect`/`Start` when a catalog stdio server's `command` binary is missing and the catalog entry declares `runtime:npm`/`package`.

**The secured design (fixes audit 4.2.1, CVSS 6.5 command injection):**
1. **Package validation:** `package` MUST match `^[a-zA-Z0-9._-]+$` + length cap (e.g. ≤214 npm-name limit) before any spawn. Reject otherwise — HARD.
2. **Allow-list:** package ∈ a curated allow-list (derive from the built-in `CATALOG` packages; registry/smithery packages NOT auto-installed unless explicitly allow-listed).
3. **Opt-in flag:** auto-install gated behind an explicit config flag (e.g. `settings.mcp.auto_install: true` or per-server `allow_auto_install: true`). Default OFF — never shells out with network on a normal reload (PORT_SPEC §6.8, §10.3 risk 6).
4. **No shell:** `exec.Command(installer, "install", package)` with `args` as a typed list, never `shell=True`. 120s timeout. Prefer `uv` over `pip`.

**Resolver order (PORT_SPEC §3.4, §8 item 23): catalog → registry → smithery.** Explicit inline config (`transport`/`command`/`url` present) bypasses ALL three. `catalog.go` does built-in catalog + shorthand→env; `registry_resolver.go` does MCP registry (phase-1.5 stub OK) + Smithery (`via:smithery`, §7.5: connect-URL vs proxy-URL, `?config=` packing, always `streamable_http` + bearer header).

---

## 8. SECURITY CHECKLIST MAPPING (PORT_SPEC §6.9)

| # | Item | Go location / owner |
|---|---|---|
| 1 | Env sandbox allow/deny at every spawn (no NODE_PATH/PYTHONPATH) | `mcp/security.go buildSafeEnv` — WORKER, at spawn boundary. (PYTHONPATH/NODE_PATH EXCLUDED — OPEN QUESTION #6 resolved.) |
| 2 | Deny-by-default sandbox gate (None blocked, empty-set allowed) | `mcp/security.go` sandbox gate — WORKER, step 1 of `executeGuarded`. Preserve `None` vs empty-set. |
| 3 | **FIX** auto-install command injection | `mcp/autoinstall.go` — WORKER (§7). |
| 4 | Path sandbox on all string args | DAEMON top-level: `workdir.EnforceArgs` (`engine.go:2614`) on `ParamSpec.Path` params + WORKER nested: `mcp/pathsandbox.go`. |
| 5 | `_source`/`_note` markers + 500 KB cap | `mcp/result.go` — WORKER. |
| 6 | Whitelist-only cache + invalidate + risk-gated semantic cache | `mcp/cache.go` — WORKER. Semantic-cache middleware = phase-1.5. |
| 7 | Strip `_INTERNAL_KEYS` | `mcp/security.go` step 4 — WORKER. |
| 8 | Risk-from-name gate → policy | Inference: `mcp/virtual.go` (`_infer_mcp_tool_risk`) → `Spec.RiskLevel`. Gate: DAEMON `Gate2Risk` via `LookupToolSpec`. |
| 9 | OAuth CSRF state + PKCE S256 + per-user stdio + encrypt-at-rest (+nonce/revocation) | DAEMON `mcp_oauth/{flow,store}.go` (CSRF/PKCE/crypto). WORKER `mcp/oauth.go` (per-user stdio pool, inject only). |
| 10 | TLS verify ON; callback 127.0.0.1; SSRF allow-list | WORKER `mcp/transport.go` (net/http default verify ON, no skip knob). Callback bind `127.0.0.1` in `mcp_oauth/routes.go`. SSRF allow-list = phase-2. |
| 11 | Two rate limiters; budget/circuit/timeout → success=false | App-level: DAEMON gate-6 `RateLimiter`. Per-server: WORKER `mcp/ratelimit.go`. `success=false` mapping in `mcp/pipeline.go`. |
| 12 | Audit logging defaults OFF for params/results; truncate `client_id[:12]` | DAEMON audit middleware (existing) + `mcp_oauth` log truncation. |

---

## 9. PHASE-1 IMPLEMENTATION PLAN (ordered, dependency-aware)

Each chunk = a coherent, testable, reviewable unit.

**Chunk 1 — Schema + compiler validation** (no worker dependency; do first).
- Files: `schema/enums.go` (fix enum), `schema/mcp.go` (NEW structs + `NormalizeServers`), `validate/mcp.go` (`CheckMCPConfig`), `compiler.go` (wire), `catalog/constraints.go` (`allowed_servers`).
- Delivers: valid MCP YAML compiles; invalid (bad transport, missing command/url, inline-no-sandbox, unknown top-level key, bad server_id, out-of-range timeout, unknown `allowed_servers`) HARD-fails with DGT-E codes.
- Test: table-driven compiler tests over the PORT_SPEC §7.4 canonical fixture (must pass) + a corpus of each invalid case (must fail with the exact code). Verify `{{workdir}}` literal in sandbox paths does NOT fail.

**Chunk 2 — JSON-RPC core + stdio transport.**
- Files: `mcp/jsonrpc.go`, `mcp/transport.go` (stdio only), `mcp/errors.go`, `mcp/security.go` (`buildSafeEnv` only).
- Delivers: connect to a real stdio server, handshake, `tools/list`, `tools/call`; id-correlation; NDJSON framing; `MCPTransportError` taxonomy + retryable classification.
- Test: spawn a fixture stdio echo-server (or `@modelcontextprotocol/server-sequential-thinking`); assert handshake + a real `tools/list`. Unit-test `buildSafeEnv` (NODE_PATH/PYTHONPATH dropped, `_BLOCKED_ENV_KEYS` dropped).

**Chunk 3 — http/streamable_http transport.**
- Files: `mcp/transport.go` (http branch).
- Delivers: POST+streaming transport; all-4-timeouts; concurrent in-flight; `http`=alias.
- Test: against a mock streamable_http server; concurrency test (N in-flight). 404→real-error (not "Session terminated").

**Chunk 4 — Pool + connect/disconnect/reconnect + health.**
- Files: `mcp/pool.go`, health loop in `module.go`.
- Delivers: single-locked pool API; store-failed-entry-anyway; atomic transport swap+rollback; backoff 1→30s+jitter; health ping + reconnect; per-server circuit breaker.
- Test: connect/reconnect/disconnect cycle; simulate refresh failure → assert rollback (old transport restored); kill a stdio server → assert health loop reconnects.

**Chunk 5 — Module skeleton + 11 meta-actions + register + worker import.**
- Files: `mcp/module.go`, `mcp/meta.go`, `mcp/register.go`, `cmd/digitorn-worker/main.go`, `mcp/config.go`.
- Delivers: `Manifest/Init/Start/Stop/Invoke`; the 11 meta-actions with aliases + exact return shapes; `health_check` 3-branch asymmetry; worker hosts `mcp`.
- Test: in-proc `Invoke` per meta-action → assert exact return envelopes (PORT_SPEC §8 items 6-13); alias dispatch; spawn the worker, `Manifests` RPC lists `mcp`.

**Chunk 6 — Virtual tools + unified guarded pipeline.**
- Files: `mcp/virtual.go`, `mcp/pipeline.go`, `mcp/result.go`, `mcp/cache.go`, `mcp/ratelimit.go`, `mcp/security.go` (rest), `mcp/pathsandbox.go`.
- Delivers: `mcp_<server>__<tool>` parse/build round-trip; `executeGuarded` with all worker-side §6.4 steps in order; normalized envelope + `_note` + 500 KB cap; whitelist cache; rate-limit; placeholder/coercion/required-params; risk-from-name materialization; OpenAI `format` sanitization.
- Test: round-trip the regex on compound ids (`mcp_google_calendar__list_events`); call a virtual tool → assert `_source`/`_note`, truncation marker, `result_count`, `status:empty`; assert both virtual AND meta-`call_tool` hit the same `executeGuarded` (e.g. rate-limit fires on both).

**Chunk 7 — Dynamic `Tools()` RPC + daemon index.**
- Files: `service/{service,types}.go`, `worker/service.go`, `proxy/proxy.go`, `domain/module/module.go` (`LiveTooler`), `server/mcp_catalog.go`, `server/registry_actions.go`, `server/bootstrap.go`.
- Delivers: `Tools()` end-to-end; `mcpCatalog`; `ForApp`/`LookupToolSpec` MCP-awareness; connect→`Tools()`→`Invalidate` index rebuild; single-dot internal FQN naming.
- Test: connect a server via meta-action → assert the next index build exposes `mcp_<server>.<tool>`; assert `LookupToolSpec("mcp_x","y")` returns a spec (gates don't fail closed); disconnect → tools vanish next build.

**Chunk 8 — Gate/approval tie-in verification (mostly free).**
- Files: none new (verify `registry_actions.go` wiring).
- Delivers: `mcp_<server>` flows all 5 gates; grant/approve/deny capabilities match; BLOCK→drop; APPROVE→approval; `allowed_servers` enforced uniformly.
- Test: the PORT_SPEC §7.4 capabilities block (grant `mcp_notion`, approve `mcp_github:delete_repo`, deny `mcp_github:merge_pr`) → assert visibility/approval/drop; assert daemon top-level path-policy confines an MCP `path` arg.

**Chunk 9 — OAuth (daemon-resolves).**
- Files: `server/mcp_oauth/{store,flow,routes,resolver}.go`, `mcp/oauth.go`, `service/types.go` (`Credentials` field), `proxy/proxy.go` (populate).
- Delivers: encrypt-at-rest; PKCE S256; CSRF state; 5 providers; per-user stdio isolation; daemon-resolves-injects; 8 bug-fixes; `requires_oauth` surfacing.
- Test: unit PKCE vectors; round-trip encrypt/decrypt; CSRF single-use+TTL; mock-provider exchange/refresh; assert `requires_oauth` shape; assert worker receives token via `Credentials`, never persists.

**Chunk 10 — Catalog + bare-ref resolution + auto-install + Smithery.**
- Files: `mcp/catalog.go`, `mcp/registry_resolver.go`, `mcp/autoinstall.go`, `mcp/sdkfix.go`, `sidecar/sdk_fix_wrapper.py`.
- Delivers: built-in catalog + shorthand→env + `_ARG_APPEND`; bare-ref 3-tier resolution; secured auto-install (validation+allow-list+opt-in); Smithery URLs; `sdk_fix_wrapper` decision logic + Python sidecar.
- Test: shorthand `github.token`→`GITHUB_PERSONAL_ACCESS_TOKEN`; reject malicious `package` (command-injection corpus); auto-install OFF by default; Smithery connect/proxy URL construction; `sdk_fix_wrapper` no-op for `node`/`npx`, wraps Python.

**Chunk 11 — E2E: Sequential Thinking + Filesystem.**
- Files: example app YAML + integration test.
- Delivers: `sequential_thinking: {}` → `npx -y @modelcontextprotocol/server-sequential-thinking` (no-auth); Filesystem (`path:` shorthand) asserting `_note` wrapper.
- Test: real worker + real servers; assert `tool_call.payload.success == True` ("did it really run"); assert `_note` verbatim on Filesystem output. Mirror PORT_SPEC §8 E2E seeds 1 & 2.

---

## 10. RISKS & OPEN GO-LEVEL QUESTIONS

**Concurrency hazards.**
- Worker `Invoke`/`Tools` are concurrent (gRPC goroutine-per-RPC, 8 HTTP/2 conns). The MCP pool, per-user pools, cache, and rate-limiters MUST each be independently concurrency-safe. Connect/disconnect/reconnect under ONE pool mutex; tool calls take only a brief RLock to snapshot the entry, then release — do NOT hold the pool lock during a (possibly 3600s) tool call or you serialise all servers.
- `mcpCatalog` (daemon) is read on every index build and written on every connect/disconnect — RWMutex + per-app generation to avoid needless rebuilds.
- Health loop vs reconnect vs an in-flight call can race on `entry.Transport` — the atomic-swap-with-rollback must be the ONLY writer of `entry.Transport`, under the pool lock.

**No Go MCP SDK — RECOMMENDATION: hand-roll JSON-RPC** (do NOT adopt `mark3labs/mcp-go` or `modelcontextprotocol/go-sdk` for phase 1). Rationale: (1) the codebase already has a proven from-scratch JSON-RPC 2.0 client (`lsp/client.go`) with exactly the id-correlation/single-reader/panic-shield pattern MCP needs; (2) the SDK quirks the spec needs to handle natively (Session-terminated-404, exception-group unwrap, FastMCP `pre_parse_json`) are *easier* to control with our own client than to patch around a third-party lib; (3) MCP's wire surface for phase-1 (initialize, tools/list, tools/call, resources, prompts, ping) is small and stable (`protocolVersion 2024-11-05`); (4) a vendored SDK adds a dependency + its own concurrency model that must be reconciled with the worker's. Cost: we own version negotiation and framing. Mitigated by `jsonrpc_test.go` against real servers.

**Proto regen tooling — NONE needed.** The contract is a hand-written `grpc.ServiceDesc` + JSON codec (`CodecName="json+module"`). Adding `Tools()` is a 4-line Go edit across `service.go`/`types.go`/`worker/service.go`/`proxy.go`. Do not look for a `.proto`.

**Wire-serialization gotcha.** The JSON codec drops `tool.Result.Diff` (json `"-"`) and raw `OutputParts.Bytes` won't survive cleanly. MCP tools returning images/binary need a blob path or base64 in `Result.Data`, not raw bytes. The `_source`/`_note`/`images`/`resources` envelope is plain JSON — fine.

**OPEN QUESTIONS needing a daemon-architecture decision:**
1. **Virtual-tool dispatch routing (§3.3).** Does the daemon send `(ModuleID="mcp", ToolName="mcp_<server>__<tool>")` for virtual tools (recommended — worker re-parses, all MCP logic worker-side), OR register `mcp_<server>` as servicebus namespaces? **Recommend the former** via a `busadapter` alias `mcp_<server>.<tool>` → `mcp` module. Needs sign-off because it touches the dispatch terminal.
2. **`InvokeRequest.Credentials` field shape.** A `map[string]string` (simple) vs a typed `AuthContext` (token, token_type, url, expires_at)? **Recommend typed `AuthContext`** for clarity + future fields. Adds a field to the wire type — confirm no other worker module breaks (it's additive/omitempty, so safe).
3. **`sdk_fix_wrapper` sidecar shipping (OPEN QUESTION #3 in spec).** `//go:embed` the Python file into the worker and materialize to temp at `Start` (self-contained binary, recommended), OR ship it as a file alongside the worker (simpler, but a deploy artifact). **Recommend `//go:embed`.** Either way it only affects **Python** stdio servers; node/npx servers never touch it.
4. **Index-rebuild trigger plumbing (§3.2).** Should the post-`connect` `Tools()`-refresh + `Invalidate` be driven by a result-metadata flag the proxy inspects, or by an explicit engine hook on meta-action return? **Recommend the metadata-flag** (`tools_changed:true`) so the worker stays the source of truth and the daemon reacts — but this needs a small engine/proxy hook decision.
5. **Per-app worker config.** `DIGITORN_MODULE_MCP_CONFIG` is process-global, NOT per-app; one worker is shared across apps routed to the pool. Per-app server config + OAuth identity MUST be resolved at call time from `InvokeRequest` identity (`AppID/UserID`), not baked into `Init`. Confirm the daemon passes per-app `servers` config — either via `Credentials`/a new `Configure` RPC, or accept that phase-1 connects only the union of all apps' declared servers and gates visibility per-app via `mcpCatalog`+`allowed_servers`. **This is the largest unresolved architectural question** — flagged for a decision before Chunk 9.

**Other risks (from PORT_SPEC §10.3):** discovery-snapshot vs worker-boundary (degraded-server-keeps-tools must survive `Tools()` — return cached tools with the `[DISCONNECTED]` hint); catalog/registry network side-effects on reload (gated behind opt-in); the FastMCP bug degrades gracefully for Python servers if the sidecar is skipped.

---

Key files for the implementer (all under `C:\Users\ASUS\Documents\digitorn_go`): `internal/modules/mcp/*` (new module), `internal/module/service/{service,types}.go` + `internal/module/worker/service.go` + `internal/module/proxy/proxy.go` (the `Tools()` RPC + `Credentials`), `internal/compiler/schema/{enums.go:411,mcp.go}` + `internal/compiler/validate/mcp.go` + `internal/compiler/compiler.go:155` + `internal/compiler/catalog/constraints.go` (schema/validation), `internal/server/{mcp_catalog.go,registry_actions.go,bootstrap.go,mcp_oauth/*}` (daemon index + OAuth), `cmd/digitorn-worker/main.go` (worker import).