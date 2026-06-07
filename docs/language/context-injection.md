# Context injection (`context:`)

Inject context into the agent's **system prompt, fresh every turn** — the user's
identity, the session's state, the date, the runtime environment, or any custom
block — declared entirely in the app manifest. This is digitorn's generalised
equivalent of the "environment block" a coding agent is handed up front (cwd, OS,
git, date), but it is **domain-agnostic**: a CRM app injects the active customer,
a support app the open ticket, a chat app nothing at all.

## Why it's a separate per-turn primitive

The base system prompt (identity, tools, operating guide, agent `system_prompt`)
is **cached per `(app, agent)`** — it never varies by user or session. Context
sections are the opposite: they carry **user- and session-specific** data, so they
are rendered **fresh on every turn and never cached**. That is the invariant that
makes injecting `user.name` / `user.region` safe — one user's data can never be
baked into another user's prompt.

It is also the discipline of a good context: inject **metadata and pointers**, not
content a tool can fetch on demand. Keep sections short; name the tool for the
detail.

## Where it goes

- **App level** — `AppDefinition.context` applies to **every agent**.
- **Agent level** — `agents[].context` is layered **on top**; a section sharing an
  `id` with an app section **replaces** it, the rest are appended.

```yaml
context:
  sections:
    - id: user
      builtin: user
      priority: 10
agents:
  - id: support
    context:
      sections:
        - id: tone          # new section, only for this agent
          text: "Be warm and apologetic."
          priority: 20
```

## A section

```yaml
- id: greeting          # optional; used to override across app/agent levels
  title: User           # optional; rendered as "# User" heading above the body
  builtin: user         # OR template OR text (see below) — first non-empty wins
  template: "..."       # a string with {{paths}} filled from the data bag
  text: "..."           # verbatim text
  when: user.region     # optional gate: render only if this path is non-empty/true
  priority: 10          # lower renders first; stable on ties
```

Exactly one **source** is used, in precedence order: `builtin` → `template` →
`text`. A section whose body renders empty (or whose `when` path is absent) is
dropped — no empty headings.

### `builtin` — ready-made sections

| Builtin | Renders |
|---|---|
| `datetime` | Current date, weekday, local time — labelled as a snapshot that does not advance during the turn. |
| `user` | "You are assisting `{name}`." + region / locale / timezone / email / roles, when present. |
| `session` | The session's goal, mode, turn number, working directory. |
| `environment` | Platform · OS · architecture (· shell) of the daemon runtime. |
| `identity` | "You are running inside the `{app}` application (version `{v}`)." |

### `template` — `{{path}}` interpolation

A `{{path}}` is replaced with its value from the **data bag**; an unknown path
becomes empty (never an error).

```yaml
- template: "You are assisting {{user.name}} ({{user.region}}, {{user.locale}}) on {{date}}."
```

### `text` — verbatim

```yaml
- title: House rules
  text: "Always answer in the user's language. Never reveal internal pricing."
```

## The data bag

| Path | Source |
|---|---|
| `user.*` | **Every claim of the caller's JWT** — `user.name`, `user.email`, `user.region`, `user.locale`, `user.roles`, `user.id`, and any custom claim (`user.tenant_id`, `user.plan`, …). |
| `app.*` | `app.id`, `app.name`, `app.version`. |
| `agent.*` | `agent.id`, `agent.role`. |
| `session.*` | `session.goal`, `session.mode`, `session.turn`, `session.workdir`. |
| `env.*` | `env.os`, `env.arch`, `env.platform` (the daemon runtime). |
| `date`, `time`, `datetime`, `weekday` | Snapshot of the turn's start time. |

> **User attributes come from the JWT.** digitorn has no user-profile store —
> identity is the validated token. To expose `user.region` / `user.locale` /
> custom fields, put them in the JWT claims. (A future builtin could read an
> external profile store.)

## `when` — conditional sections

The section renders only when the `when` path resolves to a non-empty / truthy
value. Use it to tailor context without templating logic:

```yaml
- id: gdpr
  text: "This user is in the EU — GDPR rules apply."
  when: user.region
```

## Ordering

Sections render in ascending `priority` (ties keep declaration order), each as
`# Title\nbody` (or just the body when untitled), joined by blank lines. The whole
block is appended to the system prompt after the cached sections, alongside the
working-directory and channel-context blocks.

## Sub-agents

Sub-agents carry the same caller JWT, so `user.*` resolves for them too. They see
the same app-level context; give a sub-agent its own focus with an agent-level
block.

## Full example — an advanced dev context (claude-code)

```yaml
context:
  sections:
    - id: workspace
      title: Workspace
      template: |
        Working directory: {{session.workdir}}
        Platform: {{env.platform}} · architecture {{env.arch}}.
        Every file path is relative to the working directory.
      when: session.workdir
      priority: 10
    - { id: now, builtin: datetime, priority: 11 }
    - { id: user, title: User, builtin: user, priority: 20 }
    - id: locale
      template: "Write to the user in their language ({{user.locale}}) unless they switch."
      when: user.locale
      priority: 21
    - { id: session, title: Session, builtin: session, priority: 30 }
    - id: dev_rules
      title: Working in this repository
      text: |
        - Confirm code with grep/read before you edit it or claim an API exists.
        - Fan broad search out to the explore sub-agent.
        - Keep multi-step tasks honest with task_create / task_update.
      priority: 40
```

## Full example — a non-coding app (support desk)

```yaml
context:
  sections:
    - { id: user, builtin: user, priority: 10 }
    - id: tier
      template: "Customer plan: {{user.plan}} (tenant {{user.tenant_id}})."
      when: user.plan
      priority: 20
    - id: vip
      text: "This is a VIP account — prioritise and escalate proactively."
      when: user.is_vip
      priority: 21
    - id: policy
      title: Support policy
      text: "Never promise refunds over $100 without a manager. Always confirm the account email first."
      priority: 30
```

## Implementation

- Schema: `internal/compiler/schema/context_inject.go` (`ContextBlock`,
  `ContextSection`).
- Renderer: `internal/runtime/context/ctxinject` (pure; data bag, dotted-path
  resolve, `{{}}` interpolation, builtins, `when`, priority, app/agent merge).
- Wiring: `internal/runtime/engine_context.go` decodes the caller's JWT into
  `user.*` and renders per turn; injected in `engine.go` next to the
  working-directory / channel-context blocks.
