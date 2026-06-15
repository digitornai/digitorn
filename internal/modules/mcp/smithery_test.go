package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

func smitheryFromRaw(t *testing.T, id string, raw map[string]any) (connectSpec, bool) {
	t.Helper()
	servers, _ := schema.NormalizeServers(map[string]any{id: raw})
	m := New()
	return m.resolveServer(context.Background(), id, servers[id], false)
}

func TestSmithery_ProxyURL(t *testing.T) {
	spec, ok := smitheryFromRaw(t, "github", map[string]any{
		"via": "smithery", "smithery_key": "SK",
	})
	if !ok {
		t.Fatal("smithery should resolve")
	}
	if spec.Transport != "streamable_http" {
		t.Fatalf("transport = %q", spec.Transport)
	}
	if spec.URL != "https://server.smithery.ai/@smithery-ai/github/mcp" {
		t.Fatalf("proxy url = %q", spec.URL)
	}
	if spec.Headers["Authorization"] != "Bearer SK" {
		t.Fatalf("auth header = %q", spec.Headers["Authorization"])
	}
}

func TestSmithery_NamespaceURL(t *testing.T) {
	spec, _ := smitheryFromRaw(t, "search", map[string]any{
		"via": "smithery", "smithery_key": "SK", "smithery_namespace": "my-org",
	})
	if spec.URL != "https://api.smithery.ai/connect/my-org/search/mcp" {
		t.Fatalf("connect url = %q", spec.URL)
	}
}

func TestSmithery_ConfigPacking(t *testing.T) {
	spec, _ := smitheryFromRaw(t, "github", map[string]any{
		"via": "smithery", "smithery_key": "SK", "token": "TKN",
	})
	if !strings.Contains(spec.URL, "?config=") {
		t.Fatalf("non-standard key not packed into config: %q", spec.URL)
	}
	// github maps token → GITHUB_PERSONAL_ACCESS_TOKEN, url-encoded into config.
	if !strings.Contains(spec.URL, "GITHUB_PERSONAL_ACCESS_TOKEN") {
		t.Fatalf("env_mapping not applied in smithery config: %q", spec.URL)
	}
}

func TestSmithery_MissingKeyIsSkipped(t *testing.T) {
	if _, ok := smitheryFromRaw(t, "github", map[string]any{"via": "smithery"}); ok {
		t.Fatal("smithery without a key must not resolve")
	}
}
