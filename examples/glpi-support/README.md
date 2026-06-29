# 🛠️ GLPI Support Desk

**An AI IT-support desk wired to [GLPI](https://glpi-project.org/). New tickets
arrive by webhook, an agent triages them, a specialist drafts a resolution from
*your* IT knowledge base, a human approves any write-back, and the answer is
posted to the ticket via the GLPI REST API.**

Deterministic routing, a human-in-the-loop approval gate, and a fully auditable
event trail — built on the Digitorn flow engine.

---

## Why it sells

| Buyer pain | What the desk does |
|------------|--------------------|
| Tickets pile up overnight | Triages and drafts a reply in seconds, 24/7 |
| AI bots hallucinate procedures | Answers **only** from your IT knowledge base |
| Fear of a bot replying to customers | Every public reply passes a **human approval gate** |
| One bot can't cover everything | Routes to network / software / account specialists |
| Auditors want a paper trail | Every routing + approval + write-back is a durable event |

## How it works

```
GLPI (outbound notification: new ticket)
        │  POST /hook/glpi   (X-API-Key)
        ▼
  webhook adapter (digitorn-background :8090)
        │  filter status==new · session ticket-{{id}} · owner from payload
        ▼
   triage agent ──► classifies the ticket
        ├─ network  → network specialist  ┐
        ├─ software → software specialist  │ drafts a reply from the KB
        ├─ account  → account specialist  ┘
        │                    │
        │                    ▼
        │            🔒 HUMAN APPROVAL GATE  (durable — survives a crash)
        │              ├─ approve → http.post followup + http.put status=Solved
        │              └─ reject  → escalate, left for a human
        └─ other    → human handoff
```

## What's in the box

```
glpi-support/
├── app.yaml        # 4 agents + the routing flow + approval gate + GLPI write-back + webhook channel
├── kb/             # your IT knowledge base (Markdown) — included in agent context
│   ├── network.md
│   ├── software.md
│   └── accounts.md
└── README.md
```

## Setup

1. **Install the app** (if not already):
   ```bash
   digitorn install examples/glpi-support
   digitorn app-reload glpi-support
   ```

2. **Configure from the UI** (Digitorn web / Electron):

   Open **`/agents/glpi-support`** → background console.

   | Where | Secret key | Purpose |
   |-------|------------|---------|
   | **Channels** tab | `GLPI_WEBHOOK_KEY` | Shared `X-API-Key` GLPI sends on the inbound webhook |
   | **Menu ⋮ → Secrets** | `GLPI_URL` | Base URL, e.g. `https://glpi.corp.example` |
   | **Menu ⋮ → Secrets** | `GLPI_APP_TOKEN` | GLPI API client App-Token |
   | **Menu ⋮ → Secrets** | `GLPI_SESSION_TOKEN` | Session-Token from GLPI `initSession` |

   Save each value. The webhook key is pushed to the background service
   automatically when you save in **Channels**; GLPI API credentials are
   resolved at runtime on each write-back (no restart needed).

   No LLM API key is required when using the Digitorn gateway (stay logged in).

   **Dev fallback:** the same keys can be exported as environment variables
   (`GLPI_URL`, `GLPI_APP_TOKEN`, …) before install/reload.

   **Local demo stack:** see [`demo/README.md`](demo/README.md) for Docker GLPI
   on `:8080` + scripts (`./run-demo.sh`). Temporary — delete `demo/` when done.

3. **Point GLPI at the webhook.** Configure a GLPI notification / outbound
   webhook on *ticket created* to `POST https://<your-host>/hook/glpi` with
   header `X-API-Key: <GLPI_WEBHOOK_KEY>` and a JSON body that includes at least
   `id`, `status`, `name`, `content`, `itilcategories_name`, `users_id`.
   The background service listens on `127.0.0.1:8090` by default
   (`DIGITORN_BG_HTTP_ADDR`); put it behind your reverse proxy / TLS for GLPI to
   reach it.

4. **Validate & run:**
   ```bash
   digitorn lint examples/glpi-support
   # install into a running daemon, then run digitorn-background to arm the webhook
   ```

## The demo that sells: durable, crash-safe approvals

The commercial guarantee is that **a reply is never posted to a customer's
ticket without explicit human approval**, and that the pending decision is
**durable**:

1. A ticket arrives; the flow reaches the approval gate and emits an
   `approval_request` event with `AppendDurable` (fsync) — it is on disk before
   the flow blocks.
2. Kill the daemon while the ticket is *waiting for approval*.
3. Restart: the session and the pending approval are reconstructed from the
   event log — the decision is still there, nothing was lost or auto-actioned.

This is covered end-to-end by
`internal/runtime/flow/glpi_e2e_test.go`:

- `TestGLPISupport_E2E_WritebackThroughApprovalGate` — faked GLPI ticket →
  triage → specialist → **durable** approval gate → write-back to a mocked GLPI
  (`http.post` followup + `http.put` status=Solved), asserting the App-Token /
  Session-Token headers, a valid JSON body, and the right ticket id.
- `TestGLPISupport_E2E_RejectionSkipsWriteback` — a rejected ticket makes **zero**
  GLPI calls and routes to `escalate`.

## Honest limitations (MVP, single-tenant)

These are deliberate boundaries for this first version:

- **Flow execution is not crash-resumable.** The durability guarantee is on the
  **session + the approval request** (both durable events). The in-flight flow
  *context* lives in memory: if the daemon dies mid-flow, the approval survives
  but the flow does not automatically resume from the gate — it must be
  re-driven. Durable flow resume is tracked as future work.
- **GLPI write-back body is built by string interpolation.** Specialists are
  instructed to return a single JSON-escaped string so the request body stays
  valid JSON. A dedicated GLPI tool (or JSON-encoding interpolation) would be
  more robust — future work.
- **Single-tenant.** One GLPI instance via `app.yaml` + env. Multi-tenant is
  Phase 3 (documented below).

## Phase 3 roadmap (deferred by design)

These are intentionally **not** built in this MVP. The recommended approaches are
captured here so they are ready to implement once validated:

- **Multi-tenant SaaS (recommended: webhook multiplexed by tenant).** Today there
  is one trigger per `(app, provider)`, not per-user. Rather than building
  per-user trigger arming (a large background change), keep a single
  `/hook/glpi` endpoint and carry a tenant/account identifier in the payload.
  The activation `owner` already templates over the payload
  (`owner: "{{event.payload.users_id}}"`), so each ticket's session is owned by
  the right end-user; per-tenant GLPI credentials then come from the per-user
  BYOK module-settings vault (`internal/modulesettings/`) instead of a single
  app-level `GLPI_*` env. This reuses existing ownership + BYOK plumbing.
- **Packaged GLPI connector (optional).** A first-class Activepieces piece
  (`packages/pieces/community/glpi/`: `CUSTOM_AUTH` with base_url + app_token +
  user_token, a `glpi_new_ticket` trigger, `add_followup` / `resolve` actions)
  would replace the raw webhook + `http` calls with a reusable connector, built
  via `bin/pieces-bridge/build-piece.ts glpi`. Not required — the
  webhook + `http` path already works end-to-end.

## Customize

- **Procedures** — drop your real runbooks into `kb/*.md`.
- **Routing & categories** — edit `triage`'s prompt and the `triage_node` routes.
- **What needs approval** — the approval gate currently guards every public
  reply; tighten or relax it by adding routes / conditions in the flow.
- **Model** — swap `model:` on each agent (defaults to a free model so it runs
  out of the box; point it at a premium model for production quality).
