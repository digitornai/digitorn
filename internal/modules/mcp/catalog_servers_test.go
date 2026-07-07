package mcp

import (
	"reflect"
	"testing"
)

// Per-server "toutes pièces" resolution suite. Each catalog server gets its own
// deterministic test (no network, no npx) that pins the config→launch contract:
// command, args, arg-append vs env-mapping, transport, timeout and override
// merging. Live handshake + tool calls for the same servers live in the
// mcpintegration suite (catalog_live_integration_test.go).

// filesystem — npx stdio, no secret; the `path` shorthand is appended as a
// positional directory argument (the server takes one or more allowed roots).
func TestServer_Filesystem(t *testing.T) {
	const pkg = "@modelcontextprotocol/server-filesystem"

	t.Run("bare resolves to npx/stdio", func(t *testing.T) {
		spec, ok := resolveFromRaw(t, "filesystem", map[string]any{})
		if !ok {
			t.Fatal("filesystem must resolve from the catalog")
		}
		if spec.Transport != "stdio" {
			t.Errorf("transport = %q, want stdio", spec.Transport)
		}
		if spec.Command != "npx" {
			t.Errorf("command = %q, want npx", spec.Command)
		}
		if !reflect.DeepEqual(spec.Args, []string{"-y", pkg}) {
			t.Errorf("args = %v, want [-y %s]", spec.Args, pkg)
		}
		if len(spec.Env) != 0 {
			t.Errorf("env = %v, want empty", spec.Env)
		}
		if spec.Timeout != defaultTimeout {
			t.Errorf("timeout = %v, want default %v", spec.Timeout, defaultTimeout)
		}
	})

	t.Run("path is appended as a positional arg", func(t *testing.T) {
		spec, _ := resolveFromRaw(t, "filesystem", map[string]any{"path": "/data"})
		want := []string{"-y", pkg, "/data"}
		if !reflect.DeepEqual(spec.Args, want) {
			t.Errorf("args = %v, want %v", spec.Args, want)
		}
	})

	t.Run("explicit env is merged", func(t *testing.T) {
		spec, _ := resolveFromRaw(t, "filesystem", map[string]any{
			"path": "/data", "env": map[string]any{"DEBUG": "1"},
		})
		if spec.Env["DEBUG"] != "1" {
			t.Errorf("env = %v, want DEBUG=1", spec.Env)
		}
	})

	t.Run("explicit args replace catalog args", func(t *testing.T) {
		spec, _ := resolveFromRaw(t, "filesystem", map[string]any{
			"args": []any{"--custom"},
		})
		if !reflect.DeepEqual(spec.Args, []string{"--custom"}) {
			t.Errorf("args = %v, want [--custom]", spec.Args)
		}
	})

	t.Run("explicit command bypasses the catalog", func(t *testing.T) {
		spec, ok := resolveFromRaw(t, "filesystem", map[string]any{
			"command": "my-fs-server", "args": []any{"--root", "/x"},
		})
		if !ok || spec.Command != "my-fs-server" {
			t.Fatalf("explicit command should win: %+v ok=%v", spec, ok)
		}
	})
}
