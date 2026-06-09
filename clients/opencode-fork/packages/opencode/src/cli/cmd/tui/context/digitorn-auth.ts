// Digitorn sign-in — the browser "bounce" flow, ported from the old Go CLI
// (clients/cli/internal/client/oauth.go). NOT OAuth2/PKCE : we bind a one-shot
// localhost listener, open the browser at auth.digitorn.ai/auth/oauth/{provider}
// with bounce_to=<callback>, and the auth server redirects the browser back to
// the listener with the tokens in the query string. We persist them to
// ~/.digitorn/credentials.json — the SAME file digitornConfig() already reads.
import open from "open"
import { writeFileSync, mkdirSync, readFileSync } from "node:fs"
import { homedir } from "node:os"
import { join } from "node:path"
import { createSignal } from "solid-js"

export const AUTH_PROVIDERS = ["google", "microsoft", "azure"] as const
export type AuthProvider = (typeof AUTH_PROVIDERS)[number]

// Reactive auth "tick" : UI memos that read digitornAuthState() re-evaluate when
// this bumps. refreshDigitornAuth() is called after a sign-in completes so the
// "not signed in" indicators clear without a restart.
const [authTick, setAuthTick] = createSignal(0)
export function refreshDigitornAuth(): void {
  setAuthTick((n) => n + 1)
}

export interface DigitornAuthState {
  connected: boolean
  email?: string
  name?: string
  expired: boolean
}

// digitornAuthState reports whether a usable digitorn session exists, from the same
// source digitornConfig() reads : DIGITORN_TOKEN env override, else the cached
// ~/.digitorn/credentials.json (honouring expires_at). Reactive — a memo calling
// this re-runs after refreshDigitornAuth().
export function digitornAuthState(): DigitornAuthState {
  authTick()
  if (process.env.DIGITORN_TOKEN) return { connected: true, expired: false }
  try {
    const c = JSON.parse(readFileSync(join(homedir(), ".digitorn", "credentials.json"), "utf8")) as DigitornCreds
    if (!c.access_token) return { connected: false, expired: false }
    const expired =
      typeof c.expires_at === "number" && c.expires_at > 0 && c.expires_at <= Math.floor(Date.now() / 1000)
    return { connected: !expired, email: c.email, name: c.name, expired }
  } catch {
    return { connected: false, expired: false }
  }
}

const DEFAULT_ISSUER = "https://auth.digitorn.ai"

export interface DigitornCreds {
  access_token: string
  refresh_token?: string
  expires_at?: number
  auth_url?: string
  provider?: string
  user_id?: string
  email?: string
  name?: string
}

export interface LoginHandle {
  url: string // authorize URL (shown so the user can paste it if the browser didn't open)
  done: Promise<DigitornCreds>
  cancel(): void
}

// startDigitornLogin kicks off the flow and returns immediately with the
// authorize URL + a promise that settles when the browser bounces back (or the
// flow is cancelled / times out). The dialog drives the UI from this handle.
export function startDigitornLogin(opts: {
  provider?: AuthProvider
  issuer?: string
  timeoutMs?: number
}): LoginHandle {
  const issuer = (opts.issuer || process.env.DIGITORN_AUTH_URL || DEFAULT_ISSUER).replace(/\/+$/, "")
  const provider = opts.provider ?? "google"

  let resolve!: (c: DigitornCreds) => void
  let reject!: (e: Error) => void
  const done = new Promise<DigitornCreds>((res, rej) => {
    resolve = res
    reject = rej
  })

  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch(req) {
      const u = new URL(req.url)
      if (u.pathname !== "/oauth-callback") return new Response("not found", { status: 404 })
      const q = u.searchParams
      const err = q.get("oauth_error")
      if (err) {
        reject(new Error(err))
        return new Response(errorHTML(err), htmlHeaders(400))
      }
      const access = q.get("access_token")
      if (!access) {
        reject(new Error("missing access_token in callback"))
        return new Response(errorHTML("missing access_token"), htmlHeaders(400))
      }
      const creds: DigitornCreds = {
        access_token: access,
        refresh_token: q.get("refresh_token") || undefined,
        provider: q.get("provider") || provider,
        auth_url: issuer,
      }
      const expiresIn = Number(q.get("expires_in"))
      if (Number.isFinite(expiresIn) && expiresIn > 0) {
        creds.expires_at = Math.floor(Date.now() / 1000) + expiresIn
      }
      const claims = decodeJWTClaims(access)
      if (claims) {
        creds.user_id = claims.user_id || claims.sub || ""
        creds.email = claims.email || ""
        creds.name = claims.name || claims.display_name || ""
      }
      if (!creds.user_id) creds.user_id = creds.email
      saveCredentials(creds)
      resolve(creds)
      return new Response(successHTML(), htmlHeaders(200))
    },
  })

  const callback = `http://127.0.0.1:${server.port}/oauth-callback`
  const url = `${issuer}/auth/oauth/${provider}?bounce_to=${encodeURIComponent(callback)}`
  open(url).catch(() => {}) // best-effort ; the URL is shown for manual paste

  const timer = setTimeout(
    () => reject(new Error("timed out waiting for the browser callback")),
    opts.timeoutMs ?? 180_000,
  )
  // Tear the listener down AFTER the in-flight response has flushed. Forcing it
  // (server.stop(true)) the instant `done` settles races the success-page write
  // and resets the browser socket — the login still succeeds (the token was read
  // before we returned), but the user sees "page unreachable" instead of the OK
  // page. On success we wait a tick for the bytes to leave; on error/cancel there
  // is no in-flight page, so we close immediately.
  const stop = (delayMs: number) => {
    clearTimeout(timer)
    setTimeout(() => {
      try {
        server.stop(true)
      } catch {}
    }, delayMs)
  }
  done.then(
    () => stop(500),
    () => stop(0),
  )

  return { url, done, cancel: () => reject(new Error("cancelled")) }
}

function saveCredentials(c: DigitornCreds): void {
  const dir = join(homedir(), ".digitorn")
  mkdirSync(dir, { recursive: true })
  writeFileSync(join(dir, "credentials.json"), JSON.stringify(c, null, 2), { mode: 0o600 })
}

interface JWTClaims {
  sub?: string
  user_id?: string
  email?: string
  name?: string
  display_name?: string
}

function decodeJWTClaims(token: string): JWTClaims | undefined {
  const parts = token.split(".")
  if (parts.length < 2) return undefined
  try {
    const b64 = parts[1].replace(/-/g, "+").replace(/_/g, "/")
    return JSON.parse(Buffer.from(b64, "base64").toString("utf8")) as JWTClaims
  } catch {
    return undefined
  }
}

const htmlHeaders = (status: number): ResponseInit => ({
  status,
  headers: { "content-type": "text/html; charset=utf-8", connection: "close" },
})

const successHTML = (): string =>
  `<!doctype html><html><head><meta charset="utf-8"><title>digitorn — signed in</title>
<style>body{font-family:-apple-system,BlinkMacSystemFont,sans-serif;background:#0a0a0c;color:#e8e8ea;display:flex;align-items:center;justify-content:center;height:100vh;margin:0}.card{background:#16161a;border:1px solid #2a2a30;border-radius:12px;padding:32px 40px;text-align:center;max-width:420px}h1{font-size:20px;margin:0 0 8px;color:#fff}p{margin:0;color:#9a9aa3;font-size:14px;line-height:1.5}.ok{color:#4ade80;font-size:32px;margin-bottom:12px}</style></head>
<body><div class="card"><div class="ok">✓</div><h1>You're signed in</h1><p>You can close this tab and return to your terminal.</p></div></body></html>`

const errorHTML = (msg: string): string =>
  `<!doctype html><html><head><meta charset="utf-8"><title>digitorn — sign-in failed</title>
<style>body{font-family:-apple-system,BlinkMacSystemFont,sans-serif;background:#0a0a0c;color:#e8e8ea;display:flex;align-items:center;justify-content:center;height:100vh;margin:0}.card{background:#16161a;border:1px solid #ff4e4e;border-radius:12px;padding:32px 40px;text-align:center;max-width:420px}h1{font-size:20px;margin:0 0 8px;color:#fff}p{margin:0;color:#9a9aa3;font-size:14px}code{background:#0a0a0c;padding:2px 6px;border-radius:4px;font-size:12px}.err{color:#ff4e4e;font-size:32px;margin-bottom:12px}</style></head>
<body><div class="card"><div class="err">✗</div><h1>Sign-in failed</h1><p><code>${htmlEscape(msg)}</code></p></div></body></html>`

const htmlEscape = (s: string): string =>
  s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!)
