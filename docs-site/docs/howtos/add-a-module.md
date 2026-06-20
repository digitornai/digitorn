---
id: add-a-module
title: How to add a module
---

# How to add a module

A module is a Go package that implements the `Module` interface and
registers agent-callable actions. Adding a module makes its tools
available to every app that lists it under `tools.modules.<id>` in
YAML.

This page walks through the mechanics with a minimal example.
For the full reference of action signatures, slots, capabilities,
and constraint specs, see the [Module reference index](../reference/modules/).

## Layout

```text
my_module/
├── digitorn-module.toml         # catalogue metadata
├── module.go                    # Module implementation
├── params.go                    # param type definitions
└── README.md                    # optional - dev notes
```

## 1. The module implementation

```go
package my_module

import (
    "github.com/digitorn/digitorn/pkg/module"
)

type MyModule struct {
    module.BaseModule
}

type GreetParams struct {
    Name string `json:"name" jsonschema:"minLength=1,maxLength=200,description=Person to greet."`
}

type GreetResult struct {
    Message string `json:"message"`
}

func (m *MyModule) Greet(params *GreetParams) (*module.ActionResult, error) {
    text := "Hello, " + params.Name + "!"
    return module.Success(&GreetResult{Message: text}), nil
}
```

Things to know:

- The struct embeds `module.BaseModule` to inherit the default lifecycle.
- Actions are exported methods that follow the `func (*T) ActionName(params) (*ActionResult, error)` signature.
- Params are plain Go structs with JSON tags. The daemon generates JSON Schema from them automatically.
- Return `module.Success(result)` on success or `module.Failure(err)` on error.

## 2. Register the module

The module must be registered in the daemon's module registry.
Add it to the registry in `pkg/module/registry.go` or via the
module discovery path:

```go
import "github.com/digitorn/digitorn/pkg/module"

func init() {
    module.Register("my_module", func() module.Module {
        return &MyModule{}
    })
}
```

## 3. The catalogue manifest

`digitorn-module.toml`:

```toml
[module]
id = "my_module"
name = "My Module"
version = "1.0.0"
description = "A trivial example module."
category = "general"
```

The daemon reads this at startup to populate the
module catalog

## 4. Build and verify

Rebuild the daemon:

```bash
make build
```

Verify with the CLI:

```bash
modules list | grep my_module
install my_module
```

## 5. Use it from a YAML app

```yaml
app:
  app_id: hello-app
  name: "Hello App"

agents:
  - id: assistant
    role: assistant
    brain:
      provider: ollama
      model: qwen25-7b-gpu:latest
      backend: openai_compat
      config:
        base_url: http://localhost:11434/v1
        api_key: ollama
    system_prompt: "Use the Greet tool when the user asks to be greeted."

tools:
  modules:
    my_module: {}
  capabilities:
    default_policy: auto
```

Deploy + chat:

```bash
digitorn dev deploy hello-app.yaml
digitorn dev chat hello-app -m "Greet me, I'm Paul."
```

The agent should call `my_module.Greet(name="Paul")` and respond
with the result.

## What to read next

- [Module reference index](../reference/modules/) - every shipped
  module, formatted the way yours will appear.
- [Tools reference](../language/04-tools.md) - the YAML surface
  apps use to wire your module in.
