# Digitorn Go

Go port of the Digitorn daemon — a declarative framework for building AI agent applications via YAML manifests.

## Architecture

Hexagonal (Ports & Adapters) with a modular plugin system inspired by the Python original.

```text
cmd/                    Entry points (daemon, CLI)
internal/
  domain/               Pure business types (no external deps)
  ports/                Interfaces (contracts)
  core/                 ServiceBus, EventBus, Registry, Lifecycle, Middleware pipeline
  runtime/              Agent loop, tool dispatcher
  app/                  YAML compiler -> AppDefinition
  modules/              Tool modules — filesystem & shell shipped; others on the roadmap (see ROADMAP.md)
  middleware/           13 middleware (mask_secrets, retry, etc.)
  adapters/             Concrete implementations (HTTP, Socket.IO, LLM, MCP)
  persistence/          GORM models + repositories (multi-DB: Postgres, MySQL, SQLite, MSSQL, Oracle)
  config/               Daemon configuration
  server/               Bootstrap (wires everything)
pkg/                    Public SDK (for writing modules/middleware)
```

## Stack

| Layer | Choice | Why |
|-------|--------|-----|
| HTTP | Chi + stdlib `net/http` | Zero magic, stdlib-compatible |
| Real-time | `zishang520/socket.io` v3 | Only viable Socket.IO v4+ in Go |
| ORM | GORM (multi-DB) | Officially supports Oracle, Postgres, MySQL, SQLite, MSSQL |
| Config | koanf | Lighter & cleaner than Viper |
| Logging | `slog` (stdlib) | Standard Go 1.21+ |
| Background jobs | River | Postgres-based, atomic |
| CLI | Cobra | Standard |
| TUI | Bubbletea | Modern |
| LLM | `maximhq/bifrost` gateway | One client, every provider (Anthropic, OpenAI, Ollama, …) behind a single API |
| MCP | `modelcontextprotocol/go-sdk` | Official (Google + Anthropic) |

## Quick start

```bash
# Install dependencies
make tidy

# Build
make build

# Run daemon (foreground)
make run
```

## Packaging & deployment

Digitorn is a multi-process system: the daemon (`digitornd`) spawns separate
worker executables (`digitorn-worker-llm`, `digitorn-worker-embeddings`,
`digitorn-worker`) as subprocesses. The daemon resolves each worker
**alongside its own executable**, so all binaries must be deployed together in
one folder.

Build the deployable bundle:

```bash
make dist                          # -> dist/digitorn-<version>/  (Linux/macOS/CI)
```

```powershell
pwsh scripts/package.ps1           # -> dist/digitorn-<version>-windows-amd64/ (+ .zip)
```

The bundle contains `digitornd`, the CLI, every worker, and
`config.example.yaml`. Deploy the **whole folder** — do not split the binaries.
If you must place workers elsewhere, set `workers.*.binary_path` in the config.

## Install as a service

`digitornd` is a self-installing service on Windows (SCM), Linux (systemd) and
macOS (launchd). `install` records the absolute path of `-config` and the
working directory, so the service manager always launches with the right
context. Run the install/uninstall commands from an elevated shell (admin /
`sudo`).

```bash
digitornd -config /etc/digitorn/config.yaml install   # register the service
digitornd start                                        # start it
digitornd status                                       # running | stopped
digitornd restart
digitornd stop
digitornd uninstall                                    # remove it

digitornd run                                          # foreground (dev)
```

The service restarts on failure (systemd `Restart=on-failure`, Windows
`OnFailure=restart`, launchd `KeepAlive`) and raises the systemd file-descriptor
limit (`LimitNOFILE`) for high connection concurrency.

## Configuration

Copy `config.example.yaml` to `config.yaml` and adjust.

Environment variables override config file values, prefixed with `DIGITORN_` and using `__` as nested delimiter:

```bash
DIGITORN_SERVER__PORT=9000
DIGITORN_DATABASE__URL=postgres://user:pass@localhost/digitorn
```

## License

Business Source License 1.1 (BSL). See [LICENSE](LICENSE).

Digitorn is **source-available**: free to read, modify, self-host and use
for non-production purposes. Production use is governed by the Additional
Use Grant in the license. Each release converts to Apache-2.0 on its Change
Date (four years after publication). For commercial production licensing,
contact the maintainer.
