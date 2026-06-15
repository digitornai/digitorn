package mcp

import (
	"context"
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

func resolveFromRaw(t *testing.T, id string, raw map[string]any) (connectSpec, bool) {
	t.Helper()
	servers, _ := schema.NormalizeServers(map[string]any{id: raw})
	m := New()
	return m.resolveServer(context.Background(), id, servers[id], false)
}

func TestResolve_BareRefFromCatalog(t *testing.T) {
	spec, ok := resolveFromRaw(t, "sequential_thinking", map[string]any{})
	if !ok {
		t.Fatal("bare catalog ref should resolve")
	}
	if spec.Command != "npx" || len(spec.Args) != 2 || spec.Args[1] != "@modelcontextprotocol/server-sequential-thinking" {
		t.Fatalf("bad resolved spec: %+v", spec)
	}
}

func TestResolve_EnvMappingShorthand(t *testing.T) {
	spec, ok := resolveFromRaw(t, "github", map[string]any{"token": "TKN"})
	if !ok {
		t.Fatal("github should resolve")
	}
	if spec.Env["GITHUB_PERSONAL_ACCESS_TOKEN"] != "TKN" {
		t.Fatalf("token not mapped to env: %+v", spec.Env)
	}
}

func TestResolve_ArgAppendShorthand(t *testing.T) {
	spec, ok := resolveFromRaw(t, "filesystem", map[string]any{"path": "/data"})
	if !ok {
		t.Fatal("filesystem should resolve")
	}
	last := spec.Args[len(spec.Args)-1]
	if last != "/data" {
		t.Fatalf("path not appended as positional arg: %+v", spec.Args)
	}
}

func TestResolve_ExplicitBypassesCatalog(t *testing.T) {
	spec, ok := resolveFromRaw(t, "anything", map[string]any{
		"command": "my-server", "args": []any{"--flag"},
	})
	if !ok || spec.Command != "my-server" {
		t.Fatalf("explicit config should pass through: %+v", spec)
	}
}

func TestResolve_UnknownBareRefSkipped(t *testing.T) {
	if _, ok := resolveFromRaw(t, "totally-unknown-xyz", map[string]any{}); ok {
		t.Fatal("unknown bare ref must not resolve")
	}
}

func TestResolve_StandardOverrides(t *testing.T) {
	spec, ok := resolveFromRaw(t, "github", map[string]any{
		"token": "TKN",
		"env":   map[string]any{"EXTRA": "1"},
	})
	if !ok {
		t.Fatal("resolve")
	}
	if spec.Env["EXTRA"] != "1" || spec.Env["GITHUB_PERSONAL_ACCESS_TOKEN"] != "TKN" {
		t.Fatalf("explicit env not merged over catalog: %+v", spec.Env)
	}
}
