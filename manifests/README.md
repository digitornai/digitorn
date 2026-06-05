# Module Manifests

Declarative YAML descriptors used by the compiler to validate that an app YAML
references only known modules and tools. Each file under `manifests/` describes
one module: its identity, supported platforms, tools, parameter shapes, and
configuration schema.

The compiler loads every `*.yaml` in this directory at startup. Modules
implemented in Go also advertise themselves via `module.MustRegister(...)` in
their package `init()` — both sources feed the same catalog, so a module is
"known" if either is true.

## Built-in modules (24)

Mirror the modules shipped by the Python reference daemon:

```
agent_spawn     channels         cron            filesystem      lsp
behavior        context_builder  cron_native     http            mcp
                                  database         index           memory
                                                  llm_provider    preview
                                                                  queue
                                                                  rag
                                                                  shell
                                                                  vector
                                                                  web
                                                                  web_preview
                                                                  widget
                                                                  workspace
                                                                  dev_tools
```

## Adding a custom module

Two ways to make a new module known to the compiler:

1. **Declarative** — drop a YAML manifest in this directory. The compiler will
   accept references to it, but the runtime will report "module not found"
   unless a Go implementation is also registered.

2. **Code-registered** — implement the module in Go and call
   `module.MustRegister(NewMyModule)` from its package `init()`. The runtime
   builds the manifest from the live module.

## Manifest format

```yaml
id: my_module                # snake_case, unique
version: 1.0.0
description: One-line summary shown in tooling.
supported_platforms: [linux, darwin, windows]   # omit for cross-platform
declared_permissions: [os.exec, network.http]

config_schema:               # validated against tools.modules.<id>.config
  workdir: { type: string }
  timeout: { type: duration }

constraints:                 # runtime gates declared at the manifest level
  - { name: allowed_paths, type: string_list, scope: module }
  - { name: blocked_actions, type: string_list, scope: universal }

tools:
  - name: read
    description: Read a file under the workspace.
    risk_level: low          # low | medium | high
    permissions: [filesystem.read]
    irreversible: false
    require_approval: false
    internal: false          # true to hide from agents
    tool_prompt: |           # optional, injected into the agent prompt
      Use this for...
    cli_label: Read
    cli_param: path
    tags: [io]
    aliases: [open, cat]
    params:
      - { name: path,   type: string,  required: true, description: "Relative to workspace" }
      - { name: offset, type: integer, default: 0 }
      - { name: limit,  type: integer, default: 0 }
```

### Tool parameter types

`string`, `integer`, `number`, `boolean`, `array`, `object`, `any`.

### Constraint types

`string`, `string_list`, `integer`, `boolean`, `size` (e.g. `"10MB"`),
`duration` (e.g. `"30s"`).
