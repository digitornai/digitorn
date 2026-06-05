# digitorn — native CLI/TUI client

Terminal client for the digitorn daemon. Chat with your agents, browse
sessions, install apps — all from the shell.

## Architectural contract (NON-NEGOTIABLE)

This client is a **separate Go module** (`github.com/mbathepaul/digitorn-cli`)
that lives alongside the daemon module (`github.com/mbathepaul/digitorn`)
but **shares zero Go-level coupling with it**. The compiler enforces
this because they have different `go.mod` files.

What this means concretely :

| Allowed | Forbidden |
|---|---|
| Importing `github.com/charmbracelet/*` | Importing `github.com/mbathepaul/digitorn/internal/*` |
| Importing `github.com/spf13/cobra` | Importing `github.com/mbathepaul/digitorn/pkg/*` (when those exist) |
| Hitting the daemon over REST `/api/*` | Calling daemon Go functions directly |
| Subscribing to Socket.IO events | Reading daemon DB / sessionstore files directly |
| Reading JSON event shapes from the wire | Importing daemon event type definitions |

**Why this matters** : if the CLI works against the daemon's public
contract, every other client (web, Flutter, future SDKs) MUST also
work. The CLI is the canary that proves the daemon's API surface is
clean and complete.

## Running

```bash
# From this directory
go build -o digitorn.exe ./cmd/digitorn
./digitorn.exe --help
```

Or build for release :

```bash
GOOS=linux   GOARCH=amd64 go build -ldflags "-X main.version=0.1.0" -o dist/digitorn-linux-amd64   ./cmd/digitorn
GOOS=darwin  GOARCH=arm64 go build -ldflags "-X main.version=0.1.0" -o dist/digitorn-darwin-arm64  ./cmd/digitorn
GOOS=windows GOARCH=amd64 go build -ldflags "-X main.version=0.1.0" -o dist/digitorn-windows.exe   ./cmd/digitorn
```

## Layout

```
clients/cli/
├── cmd/digitorn/main.go           Entry point, cobra+fang root
└── internal/
    ├── client/                    REST + Socket.IO wire client (no daemon imports)
    ├── config/                    ~/.digitorn/cli.toml + cli-state.json
    ├── theme/                     TOML themes (default + Catppuccin + Tokyo Night + ...)
    ├── render/                    Markdown + tool calls + diff renderers
    ├── commands/                  Batch CLI subcommands (list, install, byok, ...)
    └── tui/                       Bubble Tea fullscreen TUI
```

## Stack

- [`charmbracelet/bubbletea`](https://github.com/charmbracelet/bubbletea) v2 — TUI framework
- [`charmbracelet/bubbles`](https://github.com/charmbracelet/bubbles) v2 — Components
- [`charmbracelet/lipgloss`](https://github.com/charmbracelet/lipgloss) v2 — Styling
- [`charmbracelet/glamour`](https://github.com/charmbracelet/glamour) — Markdown rendering
- [`charmbracelet/fang`](https://github.com/charmbracelet/fang) — Cobra wrapper
- [`charmbracelet/huh`](https://github.com/charmbracelet/huh) — Interactive forms
- [`spf13/cobra`](https://github.com/spf13/cobra) — CLI router
- [`gorilla/websocket`](https://github.com/gorilla/websocket) — Socket.IO transport

## References

- [`sst/opencode@v0.6.3`](https://github.com/sst/opencode/tree/v0.6.3/packages/tui) — patterns for theme system, modal stack, SSE streaming (cloned in `vendor-references/`)
- [`charmbracelet/crush`](https://github.com/charmbracelet/crush) — active reference for daemon mode + cascading config
