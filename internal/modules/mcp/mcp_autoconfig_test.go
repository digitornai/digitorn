package mcp

import (
	"context"
	"slices"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

func TestDetectOAuthFromEnvVars(t *testing.T) {
	d := detectOAuthFromEnvVars([]string{"GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET", "GOOGLE_ACCESS_TOKEN"})
	if d == nil || d.Provider != "google" || d.ClientIDVar != "GOOGLE_CLIENT_ID" || d.TokenVar != "GOOGLE_ACCESS_TOKEN" {
		t.Fatalf("google oauth not detected: %+v", d)
	}
	if d := detectOAuthFromEnvVars([]string{"WIDGET_CLIENT_ID", "WIDGET_CLIENT_SECRET"}); d == nil || d.Provider != "custom" {
		t.Fatalf("unknown vendor must be provider=custom: %+v", d)
	}
	if detectOAuthFromEnvVars([]string{"NOTION_API_KEY"}) != nil {
		t.Error("a token-only server must NOT be inferred as OAuth")
	}
	if detectOAuthFromEnvVars(nil) != nil {
		t.Error("no env vars → nil")
	}
}

func TestEnvVarToShorthands(t *testing.T) {
	cases := map[string]string{
		"GITHUB_PERSONAL_ACCESS_TOKEN": "token",
		"FOO_ACCESS_TOKEN":             "token",
		"NOTION_API_KEY":               "api_key",
		"SLACK_BOT_TOKEN":              "bot_token",
	}
	for in, want := range cases {
		if got := envVarToShorthands(in); !slices.Contains(got, want) {
			t.Errorf("envVarToShorthands(%q) = %v, want to contain %q", in, got, want)
		}
	}
}

func TestRegistryToConnectSpec_NPM(t *testing.T) {
	srv := &registryServer{Name: "io.github.x/cool", Packages: []registryPackage{
		{RegistryType: "npm", Identifier: "@x/cool-mcp", EnvVars: []registryEnvVar{{Name: "COOL_API_KEY"}}},
	}}
	sc := schema.MCPServerConfig{Extra: map[string]any{"api_key": "sekret"}}
	spec, _, ok := registryToConnectSpec(srv, sc)
	if !ok || spec.Command != "npx" || len(spec.Args) != 2 || spec.Args[1] != "@x/cool-mcp" {
		t.Fatalf("npm spec wrong: %+v", spec)
	}
	if spec.Env["COOL_API_KEY"] != "sekret" {
		t.Errorf("shorthand credential not mapped to env var: %+v", spec.Env)
	}
}

func TestRegistryToConnectSpec_DynamicTokenWiring(t *testing.T) {
	srv := &registryServer{Name: "io.github.x/svc", Packages: []registryPackage{
		{RegistryType: "npm", Identifier: "@x/svc-mcp", EnvVars: []registryEnvVar{{Name: "SVC_ACCESS_TOKEN"}}},
	}}
	sc := schema.MCPServerConfig{Extra: map[string]any{"api_key": "xyz"}}
	spec, detected, ok := registryToConnectSpec(srv, sc)
	if !ok {
		t.Fatal("resolve failed")
	}
	if detected != nil {
		t.Errorf("a token-only server is NOT OAuth: %+v", detected)
	}
	if spec.Env["SVC_ACCESS_TOKEN"] != "xyz" {
		t.Errorf("cross-shorthand credential not wired: %+v", spec.Env)
	}
}

func TestRegistryToConnectSpec_PipAndRemote(t *testing.T) {
	pip := &registryServer{Packages: []registryPackage{{RegistryType: "pypi", Identifier: "mcp-cool"}}}
	if spec, _, ok := registryToConnectSpec(pip, schema.MCPServerConfig{}); !ok || spec.Command != "uvx" || spec.Args[0] != "mcp-cool" {
		t.Fatalf("pip spec must use uvx: %+v", spec)
	}
	rem := &registryServer{Remotes: []registryRemote{{Type: "streamable-http", URL: "https://x/mcp"}}}
	if spec, _, ok := registryToConnectSpec(rem, schema.MCPServerConfig{}); !ok || spec.Transport != "streamable_http" || spec.URL != "https://x/mcp" {
		t.Fatalf("remote spec wrong: %+v", spec)
	}
}

func TestRegistryToConnectSpec_AutoDetectsAuth(t *testing.T) {
	srv := &registryServer{Packages: []registryPackage{{RegistryType: "npm", Identifier: "@x/g",
		EnvVars: []registryEnvVar{{Name: "GITHUB_CLIENT_ID"}, {Name: "GITHUB_CLIENT_SECRET"}}}}}
	_, detected, ok := registryToConnectSpec(srv, schema.MCPServerConfig{})
	if !ok || detected == nil || detected.Provider != "github" {
		t.Fatalf("auth not auto-detected from registry env vars: %+v", detected)
	}
}

func TestMatchRegistry(t *testing.T) {
	body := []byte(`{"servers":[
		{"name":"io.github.a/other","packages":[{"registry_type":"npm","identifier":"@a/other"}]},
		{"name":"io.github.b/cool-thing","packages":[{"registry_type":"npm","identifier":"@b/cool-thing"}]}
	]}`)
	if srv := matchRegistry(body, "cool-thing"); srv == nil || srv.Name != "io.github.b/cool-thing" {
		t.Fatalf("exact match failed: %+v", srv)
	}
	if srv := matchRegistry(body, "zzz"); srv == nil || srv.Name != "io.github.a/other" {
		t.Fatalf("first-result fallback failed: %+v", srv)
	}
	wrapped := []byte(`{"servers":[{"server":{"name":"io.x/w","packages":[{"registryType":"npm","identifier":"@x/w"}]}}]}`)
	if srv := matchRegistry(wrapped, "w"); srv == nil || srv.Name != "io.x/w" {
		t.Fatalf("wrapped shape not handled: %+v", srv)
	}
}

func TestSearchRegistry_Live(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	srv, ok := searchRegistry(context.Background(), "filesystem")
	if !ok || srv == nil {
		t.Skip("registry unreachable or no match (network/API) — live path not exercised")
	}
	if len(srv.Packages) == 0 && len(srv.Remotes) == 0 {
		t.Errorf("registry hit but server has no package/remote: %+v", srv)
	}
	t.Logf("registry resolved %q with %d package(s)", srv.Name, len(srv.Packages))
}
