# 🎧 Digitorn Support Desk

**An omnichannel AI customer-support agent that answers from *your* knowledge
base, routes every request to the right specialist, and never takes a risky
action without human approval.**

Install it in minutes. Plug it into your existing channels. Cut first-response
time to seconds while keeping a human in the loop where it matters.

---

## Why it sells

| Buyer pain | What the desk does |
|------------|--------------------|
| Support tickets pile up overnight | Answers instantly, 24/7, on every channel |
| AI bots hallucinate policies | Answers **only** from your knowledge base, with citations |
| Fear of bots issuing refunds | High-value refunds **always** pass a human approval gate |
| One bot can't handle everything | Routes to specialist agents (refunds, tech, sales) |
| Setup takes weeks | One YAML file + a folder of docs |

The differentiator is the **flow engine**: routing is deterministic and
auditable, not a black box. You can see — and prove — exactly how every message
was handled.

## How it works

```
incoming message  (web chat · Telegram · Discord · webhook · voice)
        │
        ▼
   triage agent ──► classifies intent
        │
        ├─ question → knowledge-base agent (RAG) → cited answer
        ├─ refund   → refund specialist
        │               ├─ eligible & small → auto-refund
        │               ├─ high value       → 🔒 HUMAN APPROVAL GATE → refund
        │               └─ anything unclear → 🔒 HUMAN APPROVAL GATE
        ├─ bug      → tech specialist → troubleshooting / bug report
        ├─ sales    → sales specialist → plan recommendation
        └─ human    → live handoff
```

**Safe by default:** a refund is *never* silently approved. If the request is
high-value, ineligible-but-ambiguous, or the system is unsure, it goes to a
human every time.

## What's in the box

```
support-desk/
├── app.yaml        # the whole desk: 5 agents + the routing flow + RAG + gate
├── kb/             # your knowledge base (Markdown) — auto-indexed at startup
│   ├── refunds.md
│   ├── billing.md
│   └── troubleshooting.md
└── README.md
```

## Quick start

1. **Drop in your docs.** Replace the files in `kb/` with your own policies,
   FAQs, and troubleshooting guides (any `.md` files). They are embedded and
   indexed automatically on startup — no manual ingestion.

2. **Validate:**
   ```bash
   digitorn lint examples/support-desk
   ```

3. **Install into a running daemon:**
   ```bash
   curl -X POST localhost:8000/api/apps/install \
     -H "Authorization: Bearer $JWT" \
     -d '{"source": "examples/support-desk"}'
   ```

4. **Talk to it** from the web chat, or wire a channel (below).

## Channels

The desk works on the web chat with zero extra setup. To meet customers where
they are, add a channel under `tools.modules.channels` and run the background
service:

```yaml
tools:
  modules:
    channels:
      config:
        providers:
          telegram_support:
            adapter: telegram
            config:
              bot_token: "{{env.TELEGRAM_BOT_TOKEN}}"
            activation:
              flow: support              # drive the whole flow per message
              session: per_event
              message: "{{event.payload.text}}"
              reply: auto
```

Supported inbound adapters: **Telegram, Discord, Webhook, RSS, Cron**. Run
`digitorn-background` alongside the daemon to arm them.

## Customize

- **Tone & policy** — edit each agent's `system_prompt` in `app.yaml`.
- **Routing** — add or change `routes[].when` conditions in the `flow` block.
- **Approval threshold** — change the refund specialist's prompt and the
  `refund_check` routes.
- **Model** — swap `model:` on each agent (defaults to a free model so it runs
  out of the box; point it at a premium model for production quality).

## Tech notes

- Built on the Digitorn **flow engine**: deterministic routing, parallel
  fan-out, and human-in-the-loop approval gates.
- **RAG** grounds every answer in your docs (citations on).
- Every routing decision and approval is recorded as a durable event — fully
  auditable.
