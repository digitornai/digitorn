# 🎧 Digitorn Support Desk

**An omnichannel AI customer-support agent that answers from *your* knowledge
base, routes every request to the right specialist, and never takes a risky
action without human approval.**

Install it in 2 clicks. Plug it into your existing channels. Cut first-response
time to seconds while keeping a human in the loop where it matters.

---

## Why it sells

| Buyer pain | What the desk does |
|------------|--------------------|
| Support tickets pile up overnight | Answers instantly, 24/7, on every channel |
| AI bots hallucinate policies | Answers **only** from your knowledge base (RAG), with citations |
| Fear of bots issuing refunds | High-value refunds **always** pass a human approval gate |
| One bot can't handle everything | Routes to specialist agents (billing, tech, sales) |
| Bots go silent when the LLM hiccups | Per-node retries + graceful degradation to a holding reply |
| Setup takes weeks | 2 clicks + a folder of docs |

The differentiator is the **flow engine**: routing is deterministic and
auditable, not a black box. The background dashboard shows every execution as
a live workflow graph — you can see, and prove, exactly how every message was
handled.

## How it works

```
inbound (webhook from ticketing/CRM/form · Discord · WhatsApp)
   │
   ▼
triage ──question──▶ kb_agent (RAG) ──────▶ reply on the origin channel
   │──refund──▶ refund_check ──▶ [human gate if > $200] ──▶ reply
   │──bug─────▶ tech (RAG) ───────────────▶ reply
   │──sales───▶ sales (RAG) ──────────────▶ reply
   └──other───▶ human handoff
```

Replies are delivered by the **channel pipeline**, not by flow code: a webhook
request carries its own `callback_url`, a Discord message gets a thread reply,
a WhatsApp message gets a chat reply. Same flow, every channel.

## Install (2 clicks)

1. **Install the app** (store, or locally):

   ```bash
   curl -X POST http://127.0.0.1:8000/api/apps/install \
     -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -d '{"source":"/path/to/examples/support-desk"}'
   ```

2. **Paste the webhook key** (Channels tab in the UI, or):

   ```bash
   curl -X PUT http://127.0.0.1:8000/api/apps/support-desk/secrets/SUPPORT_WEBHOOK_KEY \
     -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -d '{"value":"<your key>"}'
   ```

   Saving the secret re-arms the trigger with the resolved key — no restart.
   The app's Overview shows a setup checklist until the first run lands.

## Send a request

POST anything from your ticketing, CRM, contact form or ESB:

```bash
curl -X POST http://127.0.0.1:8090/hook/support \
  -H 'X-API-Key: <your key>' -H 'Content-Type: application/json' \
  -d '{
    "id": "case-1001",
    "user_id": "<digitorn user id>",
    "subject": "Sync stopped working",
    "message": "My documents stopped syncing since yesterday...",
    "callback_url": "https://your-system/replies/case-1001"
  }'
```

The reply is POSTed to `callback_url` as `{"text": "..."}` — per request, so
each caller decides where its answer lands. Watch the run live in the app's
background dashboard (workflow canvas, approvals, reports).

## Make it yours

- **Knowledge base**: replace the files in `kb/` with your own markdown docs.
  They are indexed automatically at startup (knowledge base `company_kb`).
- **Categories**: edit the `triage` prompt + add/remove specialist agents and
  their flow branches.
- **Approval policy**: the refund threshold lives in the `refund` agent
  prompt; add gates to any branch by inserting an `approval` node.
- **More channels**: uncomment `discord` / `whatsapp` in `app.yaml` and paste
  the matching secrets — replies flow back per-channel automatically.
- **Intranet callbacks**: `allow_private_callbacks: true` lets replies target
  RFC-1918 hosts (internal ticketing); remove it if all callbacks are public.

## Secrets

| Key | Required | Purpose |
| --- | --- | --- |
| `SUPPORT_WEBHOOK_KEY` | yes | authenticates inbound webhook posts (`X-API-Key`) |
| `DISCORD_BOT_TOKEN` | if Discord enabled | bot token |
| `WHATSAPP_ACCESS_TOKEN` / `WHATSAPP_VERIFY_TOKEN` / `WHATSAPP_PHONE_ID` | if WhatsApp enabled | Meta Cloud API |
