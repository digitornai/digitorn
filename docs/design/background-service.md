# Digitorn Background Service — Design (BG-0)

Status: **DRAFT for review** · Owner: Paul · Date: 2026-06-07

A standalone, fully-isolated service that runs **background agents**: it listens
to external events (channels / triggers), and when one fires it **launches a
Digitorn agentic session by invoking the daemon's public API** — then carries the
agent's reply back out on the originating channel. It is the Go re-think of the
Python daemon's `channels` subsystem, built for **10M sessions**, **crash
survival**, and **zero coupling to the daemon**.

---

## 1. Goals / Non-goals

**Goals**
- **Perfect isolation.** The daemon is *never modified, never imported, never
  supervised by us*. The only contract is the daemon's **public HTTP API** (the
  same one the web/CLI use). Either side can be down, restarted, or replaced
  independently. Two processes, one contract.
- **Invocation-only.** Background → daemon = HTTP call (`POST /messages`).
  Daemon → background (later, for agent-initiated sends) = HTTP call to *our*
  public API. No in-process calls, no shared mutable state, no new daemon RPC.
- **Crash survival.** A kill -9 at any point loses **zero** events and produces
  **no** duplicate sessions (the three Python bugs: lost in-flight events,
  double-fire on restart, no cursor durability).
- **Scale to 10M.** Millions of armed triggers + concurrent in-flight events on
  one node, bounded memory, no goroutine-per-trigger, no goroutine-per-session.
- **Local-first, cloud-ready.** Same binary, same code: **SQLite** for a local
  daemon (one user, zero infra) ↔ **Postgres** for cloud scale. Chosen via a DSN.

**Non-goals (V1)**
- Agent-initiated outbound (`channels.send_message` as an agent tool) — V2; needs
  daemon→background invocation. V1 owns inbound + `reply: auto` only.
- Distributed multi-node scheduling — the schema supports it (leases), but V1
  targets a single background process.

---

## 2. Why not River / Temporal / Asynq

Paul's hard constraint — *ultra-puissant + simple + survit-crash + tourne en
local sur SQLite* — eliminates every off-the-shelf durable-job engine:

| Option | Durable | Local SQLite | Infra | Verdict |
|---|---|---|---|---|
| **River** | ✅ | ❌ Postgres-only (LISTEN/NOTIFY, advisory locks) | Postgres | ❌ breaks local |
| **Temporal** | ✅ | ❌ | separate server cluster | ❌ not simple/local |
| **Asynq** | ✅ | ❌ Redis-only | Redis | ❌ breaks local |
| **Pure Go on GORM** | ✅ (we build it) | ✅ SQLite ↔ ✅ Postgres | none | ✅ |

**Decision:** a **River-inspired durable-jobs core implemented in pure Go over
GORM** (the layer the daemon already uses: SQLite locally, Postgres in cloud,
same code). We get River's design (durable jobs + leases) without its Postgres
lock-in, and the daemon's proven sharded/write-behind concurrency primitives for
the hot path. No external infra to install.

---

## 3. Topology

```
        external world
   (Slack, Stripe, GitHub, cron,
    RSS, IMAP, WhatsApp, Telegram, voice…)
            │  (inbound)
            ▼
┌─────────────────────────────────────────────┐
│  digitorn-background   (standalone binary)    │   ← own process, own lifecycle
│                                               │     own port, own DB, own auth
│  Adapters → Intake(durable) → Pipeline →      │
│  Dispatcher ─────invoke (HTTP)──────────────┐ │
│  Reply  ◄────read response (HTTP/WS)───────┐│ │
└────────────────────────────────────────────┼┼─┘
                                              ││
                  daemon PUBLIC API (unchanged)││
                                              ▼▼
                         ┌──────────────────────────┐
                         │  digitornd  (UNTOUCHED)    │
                         │  POST /sessions            │
                         │  POST /sessions/{id}/msgs  │
                         │  GET  /sessions/{id}/history│
                         └──────────────────────────┘
```

The daemon has **no knowledge** of the background service. It just sees an
authenticated client posting messages. Drop the background binary, the daemon is
unaffected; drop the daemon, the background binary keeps ingesting events durably
and drains them when the daemon returns.

---

## 4. End-to-end flow (daemon untouched)

1. **Ingest.** An adapter receives an event (HTTP webhook, cron tick, poll hit…).
   It is written to the **durable intake table FIRST** (before any processing),
   tagged with a stable `dedup_key` (delivery-id / message-id / item-guid).
   Duplicate `dedup_key` → ignored (idempotent ingest). Adapter ACKs.
2. **Claim.** A worker pool **leases** the next pending job (`locked_until = now +
   lease_ttl`, atomic conditional update). Only one worker owns it.
3. **Pipeline (pure, testable).** `filter → prepare → route → build`:
   - *filter*: drop events not matching the rules.
   - *prepare*: enrich (DB/RAG lookups) → bind into the template scope.
   - *route*: pick the target app + agent from enriched data.
   - *build*: render the user message + context via safe `{{var}}` templating.
   - resolve the **session key** by strategy: `per_event` (new), `shared` (fixed),
     `template` (`ticket-{{id}}` → idempotent across retries).
4. **Invoke.** Call the daemon's public API with a **service JWT**:
   `POST /api/apps/{app}/sessions` (create if missing) → `POST …/messages`.
   The daemon runs the agentic turn **exactly as for a human** — gates, hooks,
   persistence, scale all reused, nothing duplicated.
5. **Reply (`reply: auto`).** Read the assistant's response via the daemon API
   (`GET …/history` or a Socket.IO subscription), then **send it back out** on
   the originating channel via the adapter's outbound path.
6. **Commit.** Mark the job `done`; release the lease. A crash anywhere → the
   lease expires (or replay-on-boot) → the job re-runs; the idempotent session key
   + dedup make re-runs safe.

---

## 5. Durability & crash-survival (the Python bugs, fixed)

Three invariants, each a row-level mechanism:

- **Intake-before-process.** The event is durable *before* the adapter ACKs.
  Python lost in-flight events on crash → here they survive (replayed).
- **Lease, not in-memory queue.** A job is `pending → leased(locked_until) →
  done|failed`. Worker dies → lease expires → re-leased. No "it was in a Go
  channel that vanished."
- **Dedup + idempotent session key.** `dedup_key` unique index drops duplicate
  deliveries; `template` session strategy makes a re-run hit the *same* session
  instead of spawning a second one. Python double-fired on restart → here
  at-least-once delivery + idempotent effect = effectively exactly-once.
- **Cursor durability** (pollers). RSS/IMAP cursors are columns, committed with
  the job. Restart resumes where it left off; Python re-scanned from scratch.
- **Boot recovery.** On start: re-arm triggers from the DB, requeue `leased`
  jobs whose `locked_until` is past, resume cursors. Deterministic.

---

## 6. Scale to 10M

- **No goroutine per trigger.** Triggers are *rows*; the cron scheduler and
  pollers are a small fixed set of goroutines scanning due rows. Webhook/voice
  adapters are event-driven (net/http handlers), not per-registration goroutines.
- **No goroutine per session.** Launch is an HTTP call; the daemon's
  `sessionRunner` is single-flight (no permanent goroutine per session). The
  background side holds nothing for the session lifetime.
- **Bounded worker pool** drains the jobs table (configurable parallelism), with
  **per-app fair-share quota** so one noisy app can't starve others.
- **Sharded + lock-free hot state** (reuse the daemon's proven `sync.Map`
  registries, atomic counters, write-behind) for live in-flight tracking.
- **Backpressure, not collapse.** If the daemon is saturated/down, jobs stay
  `pending` durably and drain when capacity returns — the service never melts.

---

## 7. Persistence (GORM: SQLite ↔ Postgres, same code)

```
triggers      (id, app_id, provider, adapter, config_json, enabled,
               cursor_json, created_at, updated_at)         -- armed channels
jobs          (id, trigger_id, app_id, dedup_key UNIQUE, payload_json,
               state[pending|leased|done|failed], attempts, locked_until,
               last_error, created_at, updated_at)          -- the durable queue
session_links (job_id, app_id, session_id, strategy)        -- event ↔ session
sends         (id, job_id, provider, payload_json, state, attempts)  -- outbound/reply
```
- Claim = `UPDATE jobs SET state='leased', locked_until=? WHERE id IN (SELECT … WHERE state='pending' OR (state='leased' AND locked_until<now) ORDER BY created_at LIMIT n)` — works on SQLite (`BEGIN IMMEDIATE`) and Postgres (`FOR UPDATE SKIP LOCKED`). Thin dialect shim.
- DSN selects backend: `sqlite://…/background.db` (local) or `postgres://…` (cloud).

---

## 8. The daemon contract (the ONLY coupling)

| Need | Daemon endpoint (existing, unchanged) | Auth |
|---|---|---|
| create session | `POST /api/apps/{app}/sessions` | service JWT (JWKS) |
| run a turn | `POST /api/apps/{app}/sessions/{sid}/messages` | service JWT |
| read reply | `GET …/history` or Socket.IO subscribe | service JWT |
| discover channel configs | read app bundles on disk **or** `GET /api/apps` | service JWT / fs |

**Auth:** the daemon validates JWTs via JWKS (`auth_validator.go`); a **service
account JWT** (issued by the same auth provider, machine identity) is the
credential. This is *configuration*, not daemon code. ← only open ops question.
**Nothing is added to the daemon.**

---

## 9. Adapter interface (the plug-in seam)

One small contract; every transport is "just an adapter" — webhook, cron, rss,
email, file_watcher, queue, **and** whatsapp, telegram, voice/telephony.

```go
type Event struct {
    Provider  string
    DedupKey  string          // stable per delivery — drives idempotent ingest
    Payload   map[string]any
    ReplyRef  any             // opaque handle to answer the originator
}

type Adapter interface {
    Name() string
    // Inbound: long-lived; pushes events until ctx is cancelled. Webhook/voice
    // register HTTP handlers; cron/rss/imap run a poll loop.
    Start(ctx context.Context, sink func(Event) error) error
    // Outbound: deliver a reply/send on this transport. No-op for inbound-only.
    Send(ctx context.Context, to ReplyRef, msg OutMsg) error
}
```
Messaging/telephony (WhatsApp Cloud API, Telegram Bot API, Twilio voice/SMS) are
real external integrations but **plug in without touching the core** — each is one
file implementing `Adapter`, registered in a registry. Core ships first; messaging
+ voice follow on the same seam.

---

## 10. App configuration (read by us, not pushed by the daemon)

Apps declare channels in their YAML (same shape as the Python module):
```yaml
channels:
  providers:
    my_webhook:
      adapter: webhook
      config: { inbound_path: "/hook/events", secret: "{{secret.SLACK}}" }
      activation:
        filter:  [ { field: event.payload.status, equals: "new" } ]
        route:   { field: client.plan, rules: [ {match: premium, agent: vip}, {default: true, agent: general} ] }
        session: "ticket-{{event.payload.id}}"
        reply:   auto
```
The background service **discovers** these by reading the compiled app bundles
(shared filesystem) or the daemon's `GET /api/apps` — no daemon change, no push.
Re-scan on a watch/interval to pick up installs/disables.

---

## 11. Security
HMAC signature verification (webhook), API-key gates, payload sanitization, SSRF
guard on outbound URLs, secret-filter on logs/replies, safe `{{var}}` templating
(no eval) — ported from the Python `security.py` (16 measures), reimplemented in
Go and unit-tested. The service holds channel secrets in its own store, never the
daemon's.

---

## 12. Observability
Structured logs, per-provider counters (received/filtered/launched/failed),
job-state gauges (pending/leased/failed), lease-age histogram, a `/healthz` and a
`/stats` endpoint. A future `channels.stats` agent action reads these.

---

## 13. Mapping to the Python `channels` (parity + fixes)
- adapters, activation pipeline, session strategies, reply:auto, security → **port faithfully** (the documented behaviour the user relies on).
- **fixed**: durable intake (no lost events), leases (no in-memory loss), dedup + idempotent keys (no double-fire), durable cursors, deterministic boot recovery, daemon decoupling (Python ran channels inside the daemon → fatigued it; here it's a separate process).

---

## 14. Phased plan (each phase: tested + isolated, daemon untouched)
1. **BG-1 Durable core** — GORM schema (SQLite+PG), claim/lease/dedup/cursor, boot recovery. Pure, unit + crash tests.
2. **BG-2 Skeleton binary** — `cmd/digitorn-background`, config (DSN, daemon URL, service JWT), `/healthz`, worker pool, lifecycle.
3. **BG-3 Daemon client** — thin HTTP client: create session + post message + read reply, with the service JWT. Retries/backpressure. (No daemon change.)
4. **BG-4 Pipeline** — filter/prepare/route/build + session strategies + templating + security. Pure, heavily tested.
5. **BG-5 Core adapters** — webhook (in/out) + cron. End-to-end live: event → session → reply.
6. **BG-6 Config discovery** — read `channels:` from app bundles/`/api/apps`, arm triggers, hot re-scan.
7. **BG-7 Pollers** — rss + email (cursors).
8. **BG-8 Messaging/voice** — whatsapp, telegram, twilio (same adapter seam).
9. **BG-9 Outbound-from-agent (V2)** — daemon's `channels` module → invoke background `/send` (the only daemon→background path; still pure invocation).

---

## Open questions for review
1. **Service identity**: does the auth provider issue a machine/service-account JWT the daemon's JWKS accepts? (If not, that's the one ops item — still no daemon code.)
2. **Config discovery**: read app bundles from disk (works even if daemon is down) vs `GET /api/apps` (pure API)? Lean: bundles on disk for resilience.
3. **Reply read**: poll `GET /history` (simple, robust) vs Socket.IO subscribe (lower latency)? Lean: start with poll, add socket later.
4. Single-node V1 confirmed (schema already multi-node-ready via leases)?
