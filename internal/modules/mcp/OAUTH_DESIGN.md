# MCP OAuth — Go Implementation Design (daemon-resolves)

**Behavior reference:** `internal/modules/mcp/PORT_SPEC.md` §5 (read). **Locked decisions** restated: daemon resolves+injects tokens; worker never persists secrets; 5 providers (google/github/slack/microsoft/notion) + custom; PKCE S256; CSRF state single-use 10-min TTL; encrypt access AND refresh at rest; fix the 8 §5.10 bugs; `needs_auth` surfaced as a blocking result with `metadata{requires_oauth,provider,auth_url,state,server_id}`; http→`Authorization` header, stdio→`env_token_var` restart.

**One-line architecture:** A new daemon-side package `internal/server/mcpoauth/{store,crypto,flow,resolver}.go` owns the encrypted `(user_id,provider)` token store, the PKCE/CSRF OAuth flow, the 5 real HTTP handlers (replacing the stubs at `routes.go:267-271`), and a per-call resolver. The resolver fills a **new typed `AuthContext` field on `service.InvokeRequest`** (NOT the forwarded `Config` map) keyed by `tool.Identity.UserID`; the worker re-injects it via a new `pkgmodule.WithAuthContext`, and `internal/modules/mcp` applies it as an http `Authorization` header (per-call) or a stdio `env_token_var` (per-user keyed connection).

---

## 1. BUILD-ON-EXISTING

| Need | Reuse / net-new | Cite |
|---|---|---|
| Crypto funcs (Seal/Open) | **NET-NEW.** No `internal/crypto`/`internal/secret`, no AES-GCM/secretbox/Fernet helper anywhere. `golang.org/x/crypto v0.52.0` already in `go.mod:77` (indirect) → `nacl/secretbox` available with no new dep; stdlib `crypto/aes`+`crypto/cipher` also viable. | `go.mod:77` |
| Master-key file + perms | **NET-NEW.** No `server.key`, no key mgmt. Data-dir convention to mirror: `defaultAppsRoot()` resolves `{USER_HOME}/.digitorn/apps`. New key path `{USER_HOME}/.digitorn/server.key`. | `internal/config/config.go:336` |
| DB credential model `(user_id,provider)` UNIQUE | **REUSE the table, build the store.** `models.Credential` already has the exact composite unique index `idx_cred_user_provider` + `Fields []byte (type:text)`; auto-migrated. | `internal/persistence/models/models.go:51-61`; `internal/persistence/db/migrate.go:16-31` |
| Token-store CRUD interface | **REUSE decl, impl NET-NEW.** `ports.CredentialRepository` (`Get/Set/Delete(ctx,userID,provider,...)`) is declared with the exact shape but has **zero implementations and zero call sites** (the LLM cred path at `internal/llm/bifrost/account.go:139` is a RED HERRING — JWT passthrough, never touches the DB). | `internal/ports/storage.go:5-10` |
| GORM store pattern to mirror | **REUSE pattern.** `internal/userskills/store.go` / `internal/usersnippets/store.go` (`Store{db *gorm.DB}`, `NewStore(db)`, `WithContext(ctx).Where(...).First/Create/Save/Delete`, `gorm.ErrRecordNotFound`). | `internal/usersnippets/store.go` |
| OAuth flow infra (authorize-URL builder, 5-provider table, PKCE, state store, code→token exchange, refresh) | **NET-NEW, all of it.** The only working flow (`clients/cli/internal/client/oauth.go:58`) is a non-standard `auth.digitorn.ai` browser-bounce with no PKCE/state/exchange — pattern-only reference for the CLI loopback callback (`waitForBounce`, `oauth.go:203`). Daemon→gateway auth is JWT passthrough (`internal/llm/bifrost/service.go:412-422`), not a client-credentials flow. | `clients/cli/internal/client/oauth.go:58,203` |
| chi route pattern + authed-user read | **REUSE.** The 5 MCP OAuth routes are already registered as stubs inside the authenticated group; swap `stub(...)`→real handlers. Read user via `userIDOf(r.Context())`; path params via `chi.URLParam`; respond via `writeJSON`/`writeError`. Copy `api_snippets.go`. | `routes.go:267-271`; `api_helpers.go:54,144,152` |
| http token-injection seam | **REUSE.** `headerClient`/`headerRoundTripper` — comment literally names it "the point for daemon-resolved OAuth ... on http transports." | `internal/modules/mcp/client.go:143-159` |
| stdio env-injection seam | **REUSE.** `cmd.Env = buildSafeEnv(spec.Env)` at spawn; token rides `env_token_var`, needs subprocess restart. `buildSafeEnv` must NOT drop the token var (explicit `env` overlay, not a `_BLOCKED_ENV_KEYS` entry). | `internal/modules/mcp/client.go:52-53` |
| UserID propagation (per-user key) | **REUSE, already on the wire.** `tool.Identity.UserID` → `InvokeRequest.UserID` (`proxy.go:187`, `types.go:64`) → worker re-injects (`worker/service.go:61`). | `proxy.go:186-188`; `service/types.go:64` |
| `AuthContext` wire field + `WithAuthContext` ctx helper | **NET-NEW.** No credentials field on `InvokeRequest`; no auth ctx helper next to `WithModuleConfig`. | `service/types.go:29-72`; `pkg/module/context.go` |
| MCPAuthConfig schema | **REUSE — already complete.** All §5.3 OAuth sub-fields exist: `Provider,ClientID,ClientSecret,Scopes,RedirectURI,AuthorizeURL,TokenURL,RevokeURL,PKCE *bool,TokenAuthMethod,ExtraParams,EnvTokenVar`. (PORT_SPEC §7.6's "missing fields" gap is already closed here — confirmed.) | `internal/compiler/schema/mcp.go:53-67` |

---

## 2. TOKEN STORE

**Decision: REUSE `models.Credential` (Option A) — no migration.** The `(user_id,provider)` unique index is exactly the §5.5 `UserOAuthToken` key ("two servers sharing a provider share one token"). Store the encrypted token bundle as a base64-wrapped blob in `Fields []byte (type:text)`.

- **Bundle struct (JSON, pre-encryption):** `{access_token, refresh_token, token_type, expires_at (unix), scope}`. Serialize → `json.Marshal` → `crypto.Seal` → `base64.StdEncoding` → `Fields`. `Get` reverses it. Base64-in-TEXT avoids changing the column to `bytea`/`blob` (which would be a migration). **Encrypt the whole bundle once** — access AND refresh are inside it, both covered (§5.5).
- **New store:** `internal/server/mcpoauth/store.go`, `type Store struct{ db *gorm.DB; crypto *Sealer }`, `NewStore(db, sealer)`, mirroring `internal/usersnippets/store.go` exactly. Methods:
  - `Get(ctx, userID, provider) (*Token, error)` — `WithContext(ctx).Where("user_id=? AND provider=?",...).First`; `gorm.ErrRecordNotFound`→`(nil,nil)`.
  - `Set(ctx, userID, provider, *Token)` — upsert on the composite key (`clause.OnConflict{Columns:{user_id,provider}, UpdateAll:true}` or load-or-create + `Save`).
  - `Delete(ctx, userID, provider)` — scoped delete (fixes §5.10 bug #2; see §7).
- **It MAY implement `ports.CredentialRepository`** for symmetry, but the OAuth-typed `*Token` API is cleaner than `map[string]any`; recommend a thin typed store and leave the generic port unimplemented for now. **OPEN QUESTION:** implement `ports.CredentialRepository` (first real consumer) or keep the typed store private to `mcpoauth`?
- **Migration mechanism:** Active path = GORM `AutoMigrate` at `migrate.go:16-31`, called from `bootstrap.go:126`. `Credential` is already in the list → **no migration for the token store.**
- **CSRF state store (§5.10 bug #7 — persist, not in-memory):** this is the ONE thing that needs a new table for multi-worker correctness. New model `models.OAuthState{ State (PK), UserID, AppID, Provider, ServerID, Verifier (encrypted), Nonce, RedirectURI, ExpiresAt, CreatedAt }`. **Add it to the `AutoMigrate` list** in `migrate.go` (dev) AND author `migrations/002_oauth_state.sql` with `-- +goose Up/Down` mirroring `001_init.sql` style (prod, run out-of-band; no Go code invokes goose — confirmed). The verifier is a secret → store it encrypted too. The legacy-reconcile shim (`migrate.go:43-73`) only special-cases `credentials`, so a new table is unaffected.

**OPEN QUESTION:** for single-process daemons an in-memory state map with a TTL sweeper is simpler. Persist now (multi-worker-ready, satisfies §5.10 #7) or in-memory now + flag? Recommend **persist** — it's a small table and the spec explicitly calls for it.

---

## 3. OAUTH FLOW

New file `internal/server/mcpoauth/flow.go`. All net-new (no infra to reuse except `crypto/sha256` + `encoding/base64` stdlib).

- **Provider table** `wellKnownProviders` (mirror PORT_SPEC §5.2 verbatim): google/github/slack/microsoft/notion rows with `{authorizeURL, tokenURL, pkce bool, tokenAuthMethod (body|basic), extraAuthorizeParams}`. `custom` = all URLs from YAML `MCPAuthConfig`. **Merge subtleties (§5.3):** PKCE table-override only when YAML `pkce` is true (table can turn it OFF for github/slack/notion); `token_auth_method` table-override only when current is `"body"`. notion carries `{owner:user}` extra param + `basic` exchange auth.
- **Authorize-URL build** (`buildAuthorizeURL(provider, cfg, state, codeChallenge)`): `authorize_url?response_type=code&client_id=...&redirect_uri=...&scope=<space-joined>&state=<state>` + (PKCE) `&code_challenge=<S256>&code_challenge_method=S256` + provider `extra_params` (notion `owner=user`). `redirect_uri` = the daemon callback `…/oauth/callback` (see §4).
- **PKCE S256** (§5.4): `verifier = base64url(32 random bytes)` (Go equivalent of `token_urlsafe(64)[:128]`); `challenge = base64.RawURLEncoding(sha256(verifier))`. Store `verifier` (encrypted) in the state row.
- **State (§5.4, §5.10 #7,#8):** `state = base64url(32 random bytes)`; persist `OAuthState` row bound to `(user_id, app_id, provider, server_id, verifier, nonce)`; **single-use** (delete-on-read), **10-min TTL** (`ExpiresAt = now+600s`; reject + delete if expired). A background sweeper (or lazy purge-on-access) removes expired rows. **Add a `nonce` (§5.10 #8)** even if providers don't all consume it — it's a cheap audit-recommended binding.
- **Exchange** (`exchangeCode(provider, cfg, code, verifier)`, §5.4): `basic`→`POST` JSON body + HTTP Basic auth header (client_id:client_secret); `body`→`POST` form-encoded body with `client_id`/`client_secret` inline. Always add `code_verifier` when PKCE. 30 s timeout. Parse `{access_token, refresh_token?, expires_in?, token_type?, scope?}`; `expires_at = now + expires_in`.
- **Refresh** (`refreshIfNeeded(tok, buffer=300s)`, §5.4): refresh when `now >= expires_at - 300`. **Use the SAME content-type as exchange** (fixes §5.10 bug #4 — basic providers must use the exchange content-type on refresh, not flip to/from form/json). On refresh failure return nil/force-reauth — **never serve a stale token** (§5.4).
- **TLS:** all flow HTTP uses a default `http.Client` (TLS verify ON, §6.7) — no skip-verify knob.

---

## 4. HTTP ROUTES

Replace the 5 stubs at `routes.go:267-271` with real handlers on `*Daemon` (constructed in `bootstrap.go`, see §5/§8). Handler shape copies `api_snippets.go`: `appID := chi.URLParam(r,"app_id")`, `serverID := chi.URLParam(r,"server_id")`, `userID := userIDOf(r.Context())`, respond via `writeJSON`/`writeError`.

| Method + path | Handler | Reads user via | Behavior |
|---|---|---|---|
| `GET …/oauth/authorize` | `d.mcpOAuthAuthorize` | `userIDOf(ctx)` (in auth group) | params `server_id`+`session_id`; resolve provider+cfg from app's MCP `auth:` block; gen PKCE+state; persist `OAuthState` bound to user; return `{auth_url, state, provider, server_id}`. **Never return client_secret.** |
| `GET …/oauth/callback` | `d.mcpOAuthCallback` | **`state→user` binding** (NOT the JWT) | params `code`+`state`; load+delete state row; verify TTL+single-use; exchange code (with stored verifier); `store.Set(userID,provider,tok)`; trigger index rebuild; redirect/HTML success. |
| `POST …/mcp/{server_id}/oauth-token` | `d.mcpOAuthTokenSet` | `userIDOf(ctx)` | manual token inject for `(userID, provider-of-server)`. |
| `DELETE …/mcp/{server_id}/oauth-token` | `d.mcpOAuthTokenRevoke` | `userIDOf(ctx)` | revoke — **scoped to `(userID, provider)`** (fix #2). |
| `GET …/mcp/pending-oauth` | `d.mcpPendingOAuth` | `userIDOf(ctx)` | list servers whose `(userID,provider)` token is missing/expired — **return ONLY `{server_id, provider, auth_url, requires_oauth}`, NEVER client_secret/client_id/redirect_uri** (fix #1). |

**The callback CSRF problem + the fix:** `/oauth/callback` is hit by the **provider's browser redirect**, which cannot carry the Bearer JWT — so `userIDOf(ctx)` is empty there. **Move `/oauth/callback` OUTSIDE the authenticated group**, mirroring the existing preview-token precedent (`r.With(d.panicRecoverer).Get(...)` at `routes.go:43-46`, served outside `r.Group` at `routes.go:52`). Authenticate the callback **purely via the `state→user` binding** persisted in step 1 (this IS §5.10 bug #8's required state→user CSRF binding). All other 4 routes stay inside the auth group (client-driven, carry the JWT). **OPEN QUESTION:** does the daemon issue the `redirect_uri` as its own public callback (single registered URI per provider, hosted-mode), or does the CLI use the loopback `127.0.0.1:8913/callback` Branch-A flow (§5.7)? This design covers hosted Branch-B; the CLI loopback path is a separate phase (port `waitForBounce`).

---

## 5. RESOLVE + INJECT

**The exact resolution point (keyed by UserID):** in `proxy.go` `Invoke`, **immediately after the identity copy at `proxy.go:186-188`**, before the request goes on the wire:

```
if id, ok := tool.IdentityFromContext(ctx); ok {
    req.AppID, req.SessionID, req.UserID, req.AgentID = ...   // existing line 187
    if p.authResolver != nil {                                 // NEW
        if ac := p.authResolver.Resolve(ctx, id.UserID, id.AppID, req.ModuleID, req.ToolName); ac != nil {
            req.AuthContext = ac                               // per-call, NEVER cached
        }
    }
}
```

**Why NOT the config map (KEY DECISION):** the user's just-built config-forwarding path (`appModuleConfigSource.ModuleConfig(appID, moduleID)` → `WithModuleConfig` → `InvokeRequest.Config` at `proxy.go:191-195`) is **keyed `(appID, moduleID)` only, user-agnostic, and returns the static app definition `mb.Config`** (`toolmw_source.go:39`). The mcp pool keys connections by `server_id` alone (`module.go:181`, `pool.go:81`). Injecting a per-user token into that shared map would cache user-A's token under the shared `server_id` and serve it to user-B — the exact Python multi-tenant bug §6.7 warns against. The token must ride a **separate, per-call, identity-keyed channel.**

**Net-new wire plumbing (additive, omitempty → no other worker module breaks):**
1. `internal/module/service/types.go` — add to `InvokeRequest`: `AuthContext *AuthContext json:"auth,omitempty"`; define `type AuthContext struct{ Token, TokenType, EnvTokenVar string; ExpiresAt int64 }`. (`EnvTokenVar` distinguishes http-header vs stdio-env injection target.) Optionally add to `ToolsRequest` too.
2. `internal/module/proxy/proxy.go:186-188` — the resolver call above. The resolver lives in `internal/server/mcpoauth/resolver.go`: `Resolve(ctx, userID, appID, moduleID, toolName)` → only acts for `module_id=="mcp"`; reads the server's `auth_config` (from the app definition / mcp catalog), `store.Get(userID, provider)`, `refreshIfNeeded(300s)`; returns nil when no `auth:` block (non-OAuth servers).
3. `internal/module/worker/service.go:61-77` — after the existing `WithModuleConfig` block: `if req.AuthContext != nil { ctx = pkgmodule.WithAuthContext(ctx, *req.AuthContext) }`.
4. `pkg/module/context.go` — add `WithAuthContext`/`AuthContextFrom` next to `WithModuleConfig`/`ModuleConfigFrom`.

**Reaching the worker connect spec / the two transports:**
- **http / streamable_http — per-CALL header, one shared connection per `server_id`:** `client.go:154-159` `headerRoundTripper.RoundTrip` already sets headers per request. Change it to read the token from the **call context** (`pkgmodule.AuthContextFrom(ctx)`) and overlay `Authorization: "<token_type|Bearer> <access_token>"` per request, rather than baking it into the shared `connectSpec.Headers` at dial. (`StreamableClientTransport.HTTPClient` is set once at connect — the override MUST happen inside `RoundTrip` from ctx, not by re-dialing.) This lets ONE connection serve all users; no per-user fan-out, no eviction.
- **stdio — per-USER keyed connection, restart on token change:** the token goes into subprocess **env** (`env_token_var`), baked at spawn (`client.go:52-53`), immutable for the process lifetime → it **cannot** be a per-call override. Extend the pool key from `id` to `(id, userID)` **for servers with an `auth:` block only** (non-auth stdio + all http stay shared under `server_id`). In `ensureConnected` (`module.go:174-188`), when the server has auth, set `spec.Env[authCtx.EnvTokenVar] = authCtx.Token` before `toConnectSpec`, key `m.pool.get((id,userID))`, reconnect when the user's token changes. LRU cap ≈ 20 user pools (§5.7 `_MAX_USER_POOLS`). `buildSafeEnv` must keep the token var (it's an explicit overlay, not a `_BLOCKED_ENV_KEYS` entry).

---

## 6. NEEDS_AUTH SURFACING

The `_ensure_oauth_token` decision tree (§5.7 Branch B) runs **before the external call**. In Go, the resolver (§5) decides; when it cannot produce a token it returns a **blocking signal that the mcp module turns into a `tool.Result{Success:false}`** with the canonical metadata.

- **Where:** the resolver returns either an `*AuthContext` (token ok) or a `*NeedsAuth{provider, serverID, authURL, state}` (no/expired token, refresh→nil). When `NeedsAuth` is set, the mcp module short-circuits the call in `ensureConnected`/`callTool` and returns:
  - `tool.Result{ Success:false, Error:"requires authentication", Metadata: map[string]any{ "requires_oauth":true, "provider":..., "auth_url":..., "state":..., "server_id":... } }` — verbatim §5.7 canonical needs_auth shape.
- **Building the auth_url at resolve time:** the resolver gens a fresh PKCE+state, persists the `OAuthState` row bound to `(userID, appID, provider, serverID)`, and embeds the authorize URL in the metadata — so the result is directly actionable.
- **Client trigger:** the client (web/CLI/TUI) sees `metadata.requires_oauth`, opens `auth_url` (or hits `GET …/oauth/authorize` for a fresh one), user consents, provider redirects to the daemon callback (§4), which exchanges+stores the token and rebuilds the index. The next tool call resolves a valid token. The out-of-band discovery endpoint is `GET …/mcp/pending-oauth` (now leak-free, §4).

**OPEN QUESTION (architecture seam):** the resolver decides on the **daemon side** (it owns the DB + the state store), but the blocking `tool.Result` is emitted on the **worker side** (where the mcp module runs). Cleanest: the daemon resolver, when it returns `NeedsAuth`, has the proxy short-circuit and return the blocking `tool.Result` **without crossing to the worker at all** (the worker has no token store and no business minting auth URLs). Recommend resolving + emitting the needs_auth result **in `proxy.go` / a daemon middleware**, so the worker only ever receives a valid `AuthContext` or nothing. This keeps "worker never persists/handles secrets" strict.

---

## 7. SECURITY CHECKLIST — the 8 §5.10 bugs mapped + at-rest/logging/TLS

| # | Bug (§5.10) | Fix location |
|---|---|---|
| 1 | `/pending-oauth` leaks `client_secret`/client_id/redirect_uri | `d.mcpPendingOAuth` (new, replacing `routes.go:271`): return ONLY `{server_id, provider, auth_url, requires_oauth}`. Never marshal `MCPAuthConfig.ClientSecret`. |
| 2 | DELETE token deletes ALL users | `Store.Delete(ctx,userID,provider)` scoped `WHERE user_id=? AND provider=?`; handler `d.mcpOAuthTokenRevoke` passes `userIDOf(ctx)` (replacing `routes.go:270`). |
| 3 | session-fallback reads `.id` not `user_id` | Resolver user resolution: prefer `tool.Identity.UserID` (already correct on the wire, `proxy.go:187`); any session-lookup fallback reads the `user_id` **key**, never `.id`. |
| 4 | refresh content-type asymmetry | `flow.go` `refreshIfNeeded` uses the SAME `token_auth_method`/body encoding as `exchangeCode` (basic→json+BasicAuth, body→form). |
| 5 | PKCE `is True` merge guard | `flow.go` provider-merge: table PKCE-override fires only when YAML `pkce` true; YAML-specified precedence explicit (§5.3). |
| 6 | `find_token_by_provider` shares one user's token org-wide in preload | No silent org-wide preload. Tokens resolved per-(userID,provider) at call time only; any preload is opt-in. |
| 7 | in-memory `_pending` per-process | Persisted `models.OAuthState` table (§2), single-use (delete-on-read), 10-min TTL, multi-worker-safe. |
| 8 | no nonce / no state→user binding / no real revoke | `OAuthState` row binds `state→(user_id,...)` + carries `nonce`; callback authenticates via that binding (§4); `Delete` implements real per-user revoke. |

- **Encrypt-at-rest:** access AND refresh inside the bundle, sealed via `crypto.Seal` before write; the state-row `verifier` (a secret) also encrypted. Key file `{USER_HOME}/.digitorn/server.key`, dir `0700`, file `0600`, created atomically with `os.OpenFile(path, O_RDWR|O_CREATE|O_EXCL, 0600)` (TOCTOU-safe per §5.5; perms advisory on win32 but `O_EXCL` is the real race guard). Optional env override (NOT the name `DIGITORN_SECRET_KEY` — it's already in the MCP env-denylist `_BLOCKED_ENV_KEYS`; pick a distinct name).
- **No token logging:** never log decrypted access/refresh/verifier; truncate any `client_id` to `[:12]` in audit (§6.9). The `AuthContext` field carries the token on the wire — ensure request-logging middleware does not dump `InvokeRequest` verbatim.
- **TLS verify ON** (§6.7): default `http.Client` for exchange/refresh/transport; no skip-verify knob. (SSRF guard on `url` is out of scope — trusted config by design.)

---

## 8. PHASE PLAN (ordered, testable; COLLISION RISK flagged)

**COLLISION RISK — read first.** The user is actively editing the daemon (`gitStatus` shows WIP across `internal/`, and the facets reference the user's *just-built* config-forwarding `busadapter → proxy.Config → worker WithModuleConfig`). Phases 3+ touch the **worker contract** (`service/types.go`, `proxy.go:186`, `worker/service.go:61`) and the mcp module's pool/client — the same files the user is mid-flight on. **Recommendation: do Phases 1-2 first (fully isolated, new package + DB only — zero collision), and hold Phases 3-5 until the user's WIP settles.** Before starting Phase 3, re-read `proxy.go` and `worker/service.go` to confirm the `AuthContext`-add lands cleanly next to the current identity/config lines.

| Phase | Scope | Files | How to test |
|---|---|---|---|
| **1. Store + crypto** (isolated) | `Sealer` (Seal/Open over `nacl/secretbox`), key-file mgmt; `Token` bundle; `Store` over `Credential`; `OAuthState` model + AutoMigrate + `002_oauth_state.sql` | new `internal/server/mcpoauth/{crypto.go,store.go}`; `internal/persistence/models/models.go` (+OAuthState); `internal/persistence/db/migrate.go` (AutoMigrate list); `migrations/002_oauth_state.sql` | Unit: round-trip Seal/Open; key-file O_EXCL race; store Set→Get→Delete scoped to `(user,provider)`; second user's token untouched on delete. |
| **2. Flow + routes** (isolated) | provider table, PKCE S256, state lifecycle, exchange, refresh; 5 real handlers; callback moved outside auth group | new `internal/server/mcpoauth/flow.go`; `internal/server/api_mcp_oauth.go` (handlers); `internal/server/routes.go:267-271` (swap stubs) + move callback to `routes.go:43-50` style; construct service in `bootstrap.go` near `:238` | Unit: authorize-URL per provider (table merges, PKCE on/off); state single-use+TTL; exchange basic vs body. HTTP: authorize→callback round-trip with a fake provider; pending-oauth returns no secret; DELETE scoped. |
| **3. Resolve + inject** (⚠ worker contract) | `AuthContext` wire field; resolver; ctx helper; proxy call; worker re-inject; http per-call header from ctx | `internal/module/service/types.go`; `internal/module/proxy/proxy.go:186`; `internal/module/worker/service.go:61`; `pkg/module/context.go`; `internal/modules/mcp/client.go:154` (RoundTrip reads ctx); new `internal/server/mcpoauth/resolver.go` | Integration: http MCP server requiring Bearer; two users with different tokens hit one shared connection, each request carries the right `Authorization`. |
| **4. needs_auth** | resolver returns `NeedsAuth`; proxy/daemon short-circuits to blocking `tool.Result{requires_oauth,...}`; client triggers flow | `internal/server/mcpoauth/resolver.go`; the proxy/daemon short-circuit; `internal/modules/mcp/module.go` (no token path) | Integration: call an MCP tool with no token → result `Success:false, metadata.requires_oauth=true` with a valid `auth_url`+`state`; complete callback; next call succeeds. |
| **5. Per-user stdio isolation** (⚠ pool) | pool key `(id,userID)` for auth'd stdio; env_token_var inject; reconnect-on-token-change; LRU≈20 | `internal/modules/mcp/pool.go:81` (composite key); `internal/modules/mcp/module.go:174-188` (`ensureConnected`) | Integration: stdio server with `env_token_var`; two users get separate subprocesses; token change triggers reconnect; non-auth servers stay shared. |

---

## OPEN QUESTIONS (consolidated)

1. **State store:** persist `OAuthState` (multi-worker, satisfies §5.10 #7 — recommended) vs in-memory TTL map (simpler, single-process)?
2. **Generic port:** implement `ports.CredentialRepository` (first real DB consumer) or keep a typed `mcpoauth.Store`? (Recommend typed.)
3. **redirect_uri:** hosted single daemon callback (this design, Branch-B) vs CLI loopback `127.0.0.1:8913/callback` Branch-A (`waitForBounce` port) — phase 2 covers hosted; CLI loopback is a separate phase.
4. **needs_auth emission seam:** emit the blocking `tool.Result` daemon-side in proxy (recommended — worker stays secret-free) vs worker-side in the mcp module?
5. **Refresh-cache invalidation:** §5.7 says "on refresh, invalidate server cache" — confirm where the per-user stdio reconnect + http cache invalidation hook in (ties into Phase 5 + the §6.6 result cache).
6. **`AuthContext` shape:** `{Token,TokenType,EnvTokenVar,ExpiresAt}` typed (recommended) vs bare `map[string]string` (GO_DESIGN OQ #2).

**Verified file:line anchors:** `models.Credential` `internal/persistence/models/models.go:51-61`; AutoMigrate `internal/persistence/db/migrate.go:16-31` (called `bootstrap.go:126`); `ports.CredentialRepository` `internal/ports/storage.go:5-10`; store pattern `internal/usersnippets/store.go`; data-dir `internal/config/config.go:336`; route stubs `internal/server/routes.go:267-271`; preview-outside-group `routes.go:43-50,52`; authed-user `internal/server/api_helpers.go:54,144,152`; http seam `internal/modules/mcp/client.go:143-159`; stdio env `client.go:52-53`; pool key `internal/modules/mcp/pool.go:81`; `ensureConnected` `internal/modules/mcp/module.go:174-188`; `MCPAuthConfig` (complete) `internal/compiler/schema/mcp.go:53-67`; wire types `internal/module/service/types.go:29-72,109-117`; resolve point `internal/module/proxy/proxy.go:186-195`; worker re-inject `internal/module/worker/service.go:61-77`; CLI reference flow `clients/cli/internal/client/oauth.go:58,203`; LLM red-herring `internal/llm/bifrost/account.go:139`, `service.go:412-422`.