---
id: cli-index
title: CLI reference
---

# CLI reference

The `digitorn` command is the official native TUI/CLI client for the
Digitorn daemon. It communicates exclusively over REST + Socket.IO,
with zero Go-level coupling to the daemon internals.

## Sub-command map

| Command | Purpose |
|---------|---------|
| `digitorn login` | OAuth sign-in via browser |
| `digitorn logout` | Wipe local credentials |
| `digitorn whoami` | Print current user info |
| `digitorn chat <app-id>` | Launch the TUI chat for an app |
| `digitorn list` | List installed apps |
| `digitorn sessions <app-id>` | List sessions for an app |
| `digitorn install <source>` | Install an app from a YAML path or URL |
| `digitorn uninstall <app-id>` | Remove an app |
| `digitorn enable <app-id>` | Enable a disabled app |
| `digitorn disable <app-id>` | Disable an app |
| `digitorn app-info <app-id>` | Show detailed app information |
| `digitorn app-status <app-id>` | Show app health checks |
| `digitorn app-reload <app-id>` | Recompile and reload an app from disk |
| `digitorn secret list <app-id>` | List all secret keys for an app |
| `digitorn secret get <app-id> <key>` | Get a secret value |
| `digitorn secret set <app-id> <key> [value]` | Set a secret value |
| `digitorn secret delete <app-id> <key>` | Delete a secret |
| `digitorn daemon-stats` | Show daemon-level statistics |
| `digitorn version` | Show CLI version |
| `digitorn status` | Check if the daemon is running |
| `digitorn doctor` | Environment and daemon health check |

Run any command with `--help` for the full flag list.

## Setup

```bash
digitorn login                     # OAuth sign-in via browser
digitorn whoami                    # verify you're signed in
```

## App lifecycle

```bash
digitorn install <app.yaml>        # install an app
digitorn list                      # list installed apps
digitorn uninstall <app-id>        # remove an app
digitorn enable <app-id>           # enable a disabled app
digitorn disable <app-id>          # disable an active app
digitorn app-reload <app-id>       # reload an app from disk
digitorn app-info <app-id>         # show app details
digitorn app-status <app-id>       # show app health checks
```

## Chat

```bash
digitorn chat <app-id>             # interactive TUI chat
digitorn chat <app-id> -s <sid>    # resume a specific session
digitorn sessions <app-id>         # list recent sessions
```

The TUI supports themes (Ctrl+T), session switching (Ctrl+S),
app switching (Ctrl+A), search (Ctrl+F), and abort (Ctrl+C).
See the [Client Manifest](/docs/language/44-client-manifest.md)
for theme and feature configuration.

## Secrets

```bash
digitorn secret list <app-id>                       # list keys
digitorn secret get <app-id> <key>                  # get a value
digitorn secret set <app-id> <key> <value>          # set a value
digitorn secret set <app-id> <key>                  # read from stdin
digitorn secret delete <app-id> <key>               # remove a secret
```

Secrets are stored encrypted at rest on the daemon and injected
into the agent's environment at runtime. When the value is omitted,
`secret set` reads from stdin, allowing piping of sensitive data.

## Diagnose

```bash
digitorn version                  # show CLI version
digitorn status                   # check daemon reachability
digitorn doctor                   # full environment + daemon check
digitorn daemon-stats             # daemon statistics (instance, uptime)
```

## Auth

```bash
digitorn login                     # OAuth sign-in
digitorn logout                    # clear credentials
digitorn whoami                    # current user info
```
