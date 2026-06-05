# Digitorn Go - Project Overview

## Executive Summary

**Digitorn** is a **multi-agent AI orchestration platform** written in Go. It enables LLMs to execute tasks through a modular, pluggable system of tools while maintaining strict security boundaries, permission controls, and data classification policies.

The architecture separates concerns into:
- **Daemon** (`digitornd`) — the core runtime that manages agents, modules, and tool execution
- **CLI** (`digitorn`) — command-line interface for interacting with the daemon
- **Workers** — specialized executors for compute-intensive tasks (LLM inference, embeddings)
- **Modules** — pluggable tool providers (filesystem, bash, LSP, web, etc.)

---

## Core Architecture

### 1. **Domain Layer** (`internal/domain/`)

The domain defines the contracts and types that all components respect:

#### **Agent** (`internal/domain/agent/`)
- **Role**: Classifies agent function (`worker`, `coordinator`, `specialist`)
- **Brain**: LLM provider config (provider, model, temperature, max_tokens, etc.)
- **Tools**: Declares which modules and capabilities an agent can access
- **Definition**: Compiled agent configuration with system prompt, instructions, and delegation rules

#### **Module** (`internal/domain/module/`)
- **Manifest**: Public surface of a module (ID, version, tools, dependencies, permissions, services)
- **Module Interface**: Contract every module must satisfy:
  - `Manifest()` — advertise capabilities
  - `Init(ctx, cfg)` — initialize with config
  - `Start(ctx)` — activate
  - `Stop(ctx)` — shutdown
  - `Invoke(ctx, toolName, params)` — execute a tool
- **Optional Interfaces**:
  - `Pauser` — pause/resume support
  - `Reloader` — hot config updates
  - `PromptContributor` — inject system prompt sections and dynamic tool guidance

#### **Tool** (`internal/domain/tool/`)
- **Spec**: Describes a callable tool (name, description, parameters, risk level, permissions, data classification)
- **ParamSpec**: Parameter schema (type, required, default, enum, nested properties)
- **RiskLevel**: `low`, `medium`, `high` — gates tool access based on policy
- **Irreversible**: Marks destructive operations (write, delete, etc.)
- **Path Parameters**: Filesystem paths are marked and validated against the session's workdir before dispatch
- **ToJSONSchema()**: Converts specs to JSON Schema for LLM tool definitions

#### **App** (`internal/domain/app/`)
- Application-level configuration and metadata

---

### 2. **Modules** (`internal/modules/`)

Built-in modules that provide tools to agents:

#### **Filesystem Module** (`internal/modules/filesystem/`)
- **Tools**: `read`, `write`, `edit`, `glob`, `grep`, `multi_edit`
- **Security**: Path validation enforces workdir boundaries; agents cannot escape their session directory
- **Features**: 
  - Surgical edits (by line number, text anchor, or range)
  - Batch operations (read multiple files, multi_edit atomic writes)
  - Image rendering (PNG/JPG/GIF/WEBP/BMP shown as visual content)

#### **Bash Module** (`internal/modules/bash/`)
- **Tools**: Shell command execution with session management
- **Features**:
  - Cross-platform (Windows, Linux, macOS)
  - Session persistence (maintain state across calls)
  - Environment variable management
  - Guard rails (command filtering, resource limits)

#### **LSP Module** (`internal/modules/lsp/`)
- **Language Server Protocol** integration
- Enables code intelligence (diagnostics, completions, definitions, etc.)
- Supports multiple language servers (Pyright for Python, etc.)

#### **Web Module** (`internal/modules/web/`)
- HTTP client for external API calls
- Controlled network access

---

### 3. **Server** (`internal/server/`)
- **Build**: Constructs the runtime from config
- **Dispatch**: Routes tool invocations to modules
- **Chokepoint**: Validates paths, permissions, data classification before execution
- **Lifecycle**: Manages module startup, shutdown, and hot reloads

---

### 4. **Config** (`internal/config/`)
- YAML-based configuration
- Agents, modules, permissions, data classification policies
- Example: `config.example.yaml`

---

### 5. **Daemon** (`cmd/digitornd/`)
- **Service Integration**: Runs as OS service (Windows, Linux, macOS)
- **Commands**: `install`, `uninstall`, `start`, `stop`, `restart`, `status`, `run`
- **Module Registration**: Built-in modules auto-register via `init()` imports
- **Context Cancellation**: Graceful shutdown on signals

---

### 6. **CLI** (`cmd/digitorn/`)
- Command-line interface to interact with the daemon
- Submits tasks, queries agent status, manages configuration

---

### 7. **Workers** (`cmd/digitorn-worker*`)
- **digitorn-worker**: General-purpose task executor
- **digitorn-worker-llm**: LLM inference (model serving)
- **digitorn-worker-embeddings**: Embedding generation
- Discovered and launched by the daemon via `resolveWorkerBinary()`

---

## Build & Deployment

### Makefile Targets
```bash
make build              # Build daemon + CLI + workers
make build-daemon       # Daemon only
make build-cli          # CLI only
make build-workers      # Worker binaries
make dist               # Bundle all binaries + config for deployment
make run                # Build and run daemon
make test               # Run tests with race detector
make test-coverage      # Generate coverage report
make lint               # vet + golangci-lint
make migrate-up         # Apply DB migrations (Goose)
make migrate-down       # Rollback migrations
make docker-build       # Build Docker image
```

### Distribution
- `dist/digitorn-<VERSION>/` bundles:
  - `digitornd` (daemon)
  - `digitorn` (CLI)
  - Worker binaries
  - `config.example.yaml`
  - `README.md`
- Deploy the entire folder as a unit

---

## Security Model

### 1. **Path Enforcement**
- Filesystem paths are validated at the **dispatch chokepoint** before module execution
- Agents cannot escape their session `workdir` regardless of module implementation
- Marked via `Path: true` in `ParamSpec`

### 2. **Permissions**
- Tools declare required permissions in `Spec.Permissions`
- Agents declare authorized modules in `Tools.Modules`
- Dispatch validates before invocation

### 3. **Risk Levels**
- `RiskLow`, `RiskMedium`, `RiskHigh` classify tool danger
- Policy gates execution based on agent role and app settings

### 4. **Data Classification**
- Tools declare sensitivity: `public`, `internal`, `confidential`, `restricted`
- App enforces `max_data_classification` policy
- Gate 5 blocks calls exceeding the limit

### 5. **Irreversible Operations**
- Marked via `Spec.Irreversible`
- Destructive tools (write, delete, edit) are flagged for audit/approval

---

## Key Design Patterns

### 1. **Pluggable Modules**
- Modules are discovered and registered at startup via `init()` imports
- No central registry — each module self-registers into `module.Default`
- New modules can be added by importing them in `main.go`

### 2. **Tool Specs as Contracts**
- Specs are the **single source of truth** for tool capabilities
- Converted to JSON Schema for LLM consumption
- Includes usage guidance (`ToolPrompt`), aliases for multilingual search, and tags

### 3. **Prompt Contribution**
- Modules can inject system prompt sections and dynamic tool guidance
- Scoped per app/agent to prevent information leaks
- Recomputed on prompt cache miss, not per turn

### 4. **Graceful Degradation**
- Optional interfaces (`Pauser`, `Reloader`, `PromptContributor`) allow modules to opt-in to advanced features
- Core `Module` interface is minimal and stable

### 5. **Cross-Platform Support**
- Bash module detects OS and uses native shells (PowerShell on Windows, bash on Unix)
- Makefile detects OS for binary extensions (`.exe` on Windows)
- Modules declare `SupportedPlatforms` in manifest

---

## Typical Workflow

1. **Startup**
   - Daemon loads config from YAML
   - Modules are imported and auto-register
   - Each module is initialized with its config section
   - Modules start and advertise their tools

2. **Agent Execution**
   - LLM receives agent's authorized tool list (JSON Schema)
   - LLM decides to call a tool
   - Dispatch validates: path bounds, permissions, risk level, data classification
   - Module's `Invoke()` is called with tool name and parameters
   - Result is returned to LLM

3. **Hot Reload** (optional)
   - Config is updated
   - Modules implementing `Reloader` are notified
   - Agents pick up new capabilities without restart

4. **Shutdown**
   - Modules are stopped gracefully
   - Daemon exits

---

## Dependencies

### Core
- `github.com/kardianos/service` — OS service integration
- `go.uber.org/automaxprocs` — Auto-detect CPU count

### Database
- `github.com/pressly/goose/v3` — Schema migrations

### Linting
- `github.com/golangci/golangci-lint` — Code quality

### Testing
- Standard `testing` package with race detector

---

## File Structure

```
digitorn_go/
├── cmd/
│   ├── digitornd/          # Daemon entrypoint
│   ├── digitorn/           # CLI entrypoint
│   ├── digitorn-worker/    # General worker
│   ├── digitorn-worker-llm/
│   └── digitorn-worker-embeddings/
├── internal/
│   ├── domain/             # Core types & contracts
│   │   ├── agent/
│   │   ├── app/
│   │   ├── module/
│   │   └── tool/
│   ├── modules/            # Built-in modules
│   │   ├── bash/
│   │   ├── filesystem/
│   │   ├── lsp/
│   │   └── web/
│   ├── server/             # Runtime & dispatch
│   ├── config/             # Configuration loading
│   └── version/            # Version info
├── migrations/             # Database schema (Goose)
├── Makefile                # Build targets
├── config.example.yaml     # Example configuration
├── go.mod / go.sum         # Dependencies
└── README.md               # Project documentation
```

---

## Next Steps for Development

1. **Add a New Module**: Create `internal/modules/mymodule/module.go`, implement `Module` interface, import in `main.go`
2. **Add a Tool**: Define `tool.Spec` in module manifest, implement `Invoke()` handler
3. **Extend Security**: Add new risk levels, permissions, or data classifications in domain types
4. **Deploy**: Run `make dist` and deploy the bundle folder
5. **Test**: Run `make test` with race detector; add module-specific tests

---

## Summary

Digitorn is a **production-grade, modular AI agent orchestration platform** that:
- ✅ Isolates agents in secure sandboxes (workdir boundaries, permission checks)
- ✅ Provides rich tool ecosystem (filesystem, bash, LSP, web, extensible)
- ✅ Enforces data classification and risk policies
- ✅ Supports multi-agent coordination (roles, delegation)
- ✅ Runs as an OS service (Windows, Linux, macOS)
- ✅ Scales via worker processes (LLM, embeddings)
- ✅ Hot-reloads configuration without downtime

It's designed for **enterprise AI applications** where safety, auditability, and modularity are paramount.
