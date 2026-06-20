---
id: install
title: Install
sidebar_label: Install
sidebar_position: 0
---

Digitorn is a multi-process system: the daemon (`digitornd`) spawns separate
worker executables as subprocesses. All binaries must be deployed together.

## Download a release

Pre-built binaries are available on the [GitHub Releases page](https://github.com/mbathepaul/digitorn/releases).

Each release ships a bundle containing:

- `digitornd` — the daemon process (HTTP API, Socket.IO events, agent loop)
- `digitorn` — the CLI client (chat, app management, auth)
- `digitorn-worker-llm` — LLM inference worker
- `digitorn-worker-embeddings` — embedding worker
- `digitorn-worker` — general-purpose worker
- `digitorn-background` — background automation service (optional)
- `config.example.yaml`

Deploy the **whole folder** — do not split the binaries.

### Linux / macOS

```bash
# Download and extract the latest bundle for your platform
curl -fsSL https://github.com/mbathepaul/digitorn/releases/latest/download/digitorn-linux-amd64.tar.gz | tar xz
cd digitorn-*
```

### Windows

```powershell
# Download the latest Windows bundle
Invoke-WebRequest -Uri https://github.com/mbathepaul/digitorn/releases/latest/download/digitorn-windows-amd64.zip -OutFile digitorn.zip
Expand-Archive -Path digitorn.zip -DestinationPath digitorn
cd digitorn
```

## Build from source

```bash
git clone https://github.com/mbathepaul/digitorn
cd digitorn

# Build everything (daemon + CLI + workers)
make build

# Or build individual components
make build-daemon       # digitornd only
make build-cli          # digitorn (CLI) only
make build-workers      # worker binaries only

# Build a deployable bundle
make dist               # -> dist/digitorn-<version>/
```

The Windows host can cross-compile for Linux:

```powershell
.\build.ps1
```

## Run the daemon

### Foreground (development)

```bash
# Build and run with default config
make run

# Or manually
digitornd -config config.yaml run
```

### Install as a service (production)

`digitornd` is a self-installing service on Windows (SCM), Linux (systemd) and
macOS (launchd). `install` records the absolute path of `-config` and the
working directory, so the service manager always launches with the right
context.

```bash
# Register the service (run with sudo / admin)
digitornd -config /etc/digitorn/config.yaml install

# Lifecycle
digitornd start
digitornd status
digitornd restart
digitornd stop

# Remove the service
digitornd uninstall

# Foreground (dev)
digitornd -config config.yaml run
```

The service restarts on failure (systemd `Restart=on-failure`, Windows
`OnFailure=restart`, launchd `KeepAlive`) and raises the systemd file-descriptor
limit (`LimitNOFILE`) for high connection concurrency.

## Configuration

Copy `config.example.yaml` to `config.yaml` and adjust.

Environment variables override config file values, prefixed with `DIGITORN_`
and using `__` as nested delimiter:

```bash
DIGITORN_SERVER__PORT=9000
DIGITORN_DATABASE__URL=postgres://user:pass@localhost/digitorn
```

## Verifying

```bash
# Check the daemon is running
curl http://127.0.0.1:8000/health

# List installed apps via the CLI
digitorn list
```

## What gets installed

| Location | Contents |
|----------|----------|
| Bundle directory (`digitorn-<version>/`) | Daemon + CLI + worker binaries, `config.example.yaml` |
| `~/.digitorn/` | Per-user data: `config.yaml`, `digitorn.db`, `apps/`, `sessions/`, `logs/` |
| `~/.cache/digitorn/` (or platform equivalent) | Embedding model weights (~220 MB for the default model) |
| systemd unit / LaunchAgent / Windows Service | Background service registration |

## Prerequisites

- **Go 1.22+** (only if building from source)
- **Git** (only if building from source)
- **Optional**: Node.js + npx for MCP servers that require them

## Uninstall

```bash
# Stop and remove the service
digitornd uninstall

# Remove the data directory
rm -rf ~/.digitorn
```

## Troubleshooting

### `digitorn` not on PATH

Add the bundle directory to your PATH, or symlink the binaries:

```bash
ln -s $(pwd)/digitorn ~/.local/bin/digitorn
ln -s $(pwd)/digitornd ~/.local/bin/digitornd
```

### Service won't start

```bash
digitornd status             # show current state
sudo journalctl -u digitornd  # systemd logs (Linux)
```

Common causes: port already in use, missing config file, worker binaries not
found alongside `digitornd`.

### Port 8000 already in use

Change the port in `config.yaml`:

```yaml
server:
  port: 9000
```

Or via environment variable:

```bash
DIGITORN_SERVER__PORT=9000 digitornd -config config.yaml run
```

### Linux: service stops when I log out

User systemd units stop with the user session unless lingering is enabled:

```bash
sudo loginctl enable-linger $USER
```
