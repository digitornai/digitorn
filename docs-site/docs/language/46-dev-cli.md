---
id: dev-cli
---

# App lifecycle & chat

The Go CLI provides commands for developing and testing apps.
They hit the same HTTP endpoints the chat client / web client
uses, so every behavior rule, sub-agent,
tool call, and approval flow runs in production mode · no
mocks.

| Command | Purpose |
|---------|---------|
| `digitorn install <source>` | Install an app YAML on the running daemon. |
| `digitorn chat <app-id>` | Multi-turn chat with the installed app (auto-approval). |
| `digitorn list` | List installed apps and their status. |
| `digitorn uninstall <app-id>` | Remove an installed app. |

All commands accept `--daemon` (default
`http://127.0.0.1:8000`).

## Prerequisites

```bash
# Start the daemon in another terminal
digitornd -config config.yaml run

# OR talk to a remote daemon
digitorn <command> --daemon https://my-daemon.example.com
```

JWT auth is honoured - the CLI threads the cached token from
`~/.digitorn/credentials.json` (set by `digitorn login` or
the init flow). When auth is disabled on the daemon
(`server.auth_enabled: false`), no token is needed.

## `digitorn dev deploy`

 Deploys an app YAML to the running daemon.

```bash
digitorn dev deploy path/to/app.yaml \
  [--daemon http://127.0.0.1:8000] \
  [--force/--no-force] \
  [--scope system|user]
```

| Flag | Default | Effect |
|------|---------|--------|
| `--daemon`, `-d` | `http://127.0.0.1:8000` | Daemon URL. |
| `--force` / `--no-force` | `--force` (true) | Overwrite an existing deployment with the same `app_id`. |
| `--scope`, `-s` | `system` | Install scope. `system` requires admin; `user` deploys a private copy visible only to your JWT identity. |

The CLI calls the daemon's deploy endpoint with
`{yaml_path: <abs>, force: true|false}`. The daemon compiles
the YAML in place (no upload - the path must be reachable
from the daemon's filesystem) and prints:

```
Deployed: my-app (conversation mode)
  Agents: assistant, reviewer
```

Failure surfaces the compiler's error message (schema
violations, missing references, ...).

## `digitorn dev status`

 Print the deployment status of an app.

```bash
digitorn dev status my-app
```

Calls the daemon's app-detail endpoint and prints:

```
App: my-app
  Status: active
  Mode: conversation
  Agents: ['assistant', 'reviewer']
```

`Not found` (red) when the app isn't deployed.

## `digitorn dev history`

 Print the full message history of a session.

```bash
digitorn dev history my-app abc123-session-id
```

Calls the daemon's session-history endpoint and renders each
message colour-coded by role:

| Role | Format |
|------|--------|
| `system` | Blue, truncated to first 100 chars. |
| `user` | Green, prefixed with `>`. |
| `assistant` | Cyan, with `(used N tools)` if there were tool calls, plus the formatted tool call list. |
| `tool` | Yellow, prefixed with `-> result (tcid):` and truncated to first 100 chars. |

Useful for debugging what the agent actually said and which
tools it called, after the fact.

## `digitorn dev chat`

 The flagship command - interactive multi-turn
chat with auto-approval.

```bash
# Interactive (recommended for exploration)
digitorn dev chat my-app

# With a specific workspace path injected as a session metadata
digitorn dev chat my-app --workspace /path/to/project

# Resume an existing session
digitorn dev chat my-app --session abc123

# Custom daemon
digitorn dev chat my-app --daemon https://prod.example.com

# Single message (script-friendly, non-interactive)
digitorn dev chat my-app -m "What's in src/auth/?"
```

| Flag | Default | Effect |
|------|---------|--------|
| `--workspace`, `-w` | `""` | Workspace directory path (passed as session metadata). |
| `--daemon`, `-d` | `http://127.0.0.1:8000` | Daemon URL. |
| `--session`, `-s` | `""` (new session) | Resume an existing session id. |
| `--timeout`, `-t` | `600.0` | Max seconds to wait per turn before declaring a timeout. |
| `--message`, `-m` | `""` (interactive) | Single message; non-interactive - sends, waits, prints, exits. Script-friendly. |

### Interactive mode

```
[my-app] session abc123
> analyze this project
(waiting for agent...)
  [auto-approved] shell.bash
  [auto-approved] filesystem.read
Assistant: Looking at the project structure, I see ...
> /quit
```

Built-in commands inside the chat prompt:

| Command | Effect |
|---------|--------|
| `/quit`, `/exit` | End the session and exit. |
| `/abort` | Cancel the current in-flight turn. |

### What auto-approval does

`_auto_approve_pending`. Every turn the CLI polls
the daemon's pending-approvals endpoint. Any pending request
from `tools.capabilities.approve` is auto-approved with
`{request_id, approved: true}`.

This sidesteps the human-in-the-loop pause that production
clients honour - useful for **automated testing** (the agent
can exercise destructive actions without a human stopping it).

> **Don't use `digitorn dev chat` for production demos** that
> rely on the approval gate as a safety net. The auto-approval
> is part of the testing contract; production clients keep
> the gate strict.

For sane testing without auto-approval, deploy an app with
`tools.capabilities.default_policy: auto` (no approval needed
on any tool) and skip the gate entirely instead.

### Polling and timeouts

`_poll_until_done`. After each user message, the
CLI polls the session state every second until `is_active`
flips to `false`. The default timeout is **600 seconds (10
min)**. Override with `-t 1800` for slower tasks.

If the timeout elapses, the CLI prints the warning, the turn
keeps running on the daemon - you can resume the session and
poll again, or run `digitorn dev history` to see what was
produced.

## Programmatic API

`dev_cli` can be invoked programmatically:

```python
from digitorn.core.cli.dev import dev_cli

# Equivalent to: digitorn dev chat my-app -m "test"
dev_cli(["chat", "my-app", "-m", "test"])
```

Used by the Builder agent to deploy + smoke-test apps it
command - wrap in a try/except `SystemExit` to keep the parent
process alive.

## Common workflows

### Test an app you just edited

```bash
digitorn dev deploy ./my-app.yaml      # compile + deploy
digitorn dev chat my-app -m "test"     # smoke test
digitorn dev history my-app <session>  # inspect what happened
```

### CI smoke test

```bash
#!/bin/bash
set -e
digitorn dev deploy "$YAML_PATH" --no-force        # fails if already deployed
digitorn dev chat "$APP_ID" -m "$SMOKE_MESSAGE" --timeout 120
echo "Smoke test passed"
```

### Builder loop

```bash
# A builder agent writes YAML → deploys → smoke tests → reads
# history → fixes. Wraps each step in dev_cli()
# programmatically; checks _state/compile.json and
# _state/tests.json for outcomes.
```

## Daemon API surface used

The CLI is an HTTP wrapper around the daemon's apps,
sessions, messages, and approvals surfaces. Each command
calls one or two endpoints:

| Command | What it calls |
|---------|---------------|
| `deploy` | App-deploy endpoint. |
| `status` | App-detail endpoint. |
| `history` | Session-history endpoint. |
| `chat` | Session-create + message-post + session-poll + approvals-poll + approve-resolve (auto-approval loop). |

The exact route shapes are not documented publicly -
external integrators should use the native CLI or the chat client
which abstracts these calls.

## Cross-references

- Top-level CLI overview (deploy, app schema, secret ...):
  [CLI Reference](/docs/reference/cli/)
  [Security → Resolving a policy](11-security.md#resolving-a-policy)
- Auth + JWT cache (`~/.digitorn/credentials.json`):
  [Auth](22-auth.md)
- Background mode triggers (`dev chat` doesn't drive triggers
  - those run on the daemon's schedule):
  [Triggers](09-triggers.md)
