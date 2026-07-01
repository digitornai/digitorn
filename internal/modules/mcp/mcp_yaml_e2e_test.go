//go:build mcpintegration

// Ultra E2E: a REAL app YAML declaring every kind of MCP server (bare catalog
// npm, catalog shorthand, arg-append, smithery, explicit http), compiled through
// the REAL compiler, every server resolved through the real catalog/smithery
// path, and finally the agent calling a tool on a REAL running MCP server that
// was configured purely from the YAML.
//
// Run: go test -tags mcpintegration ./internal/modules/mcp/ -run YAMLApp -v
package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/digitornai/digitorn/internal/compiler"
	compilercatalog "github.com/digitornai/digitorn/internal/compiler/catalog"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
	pkgmodule "github.com/digitornai/digitorn/pkg/module"
)

type yamlEchoArgs struct {
	Text string `json:"text"`
}

func startPlainMCPServer(t *testing.T) string {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "yaml-e2e", Version: "v1"}, nil)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "echo", Description: "echo back"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, a yamlEchoArgs) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "echo:" + a.Text}}}, nil, nil
		})
	handler := mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return srv },
		&mcpsdk.StreamableHTTPOptions{JSONResponse: true},
	)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestYAMLApp_AllServerTypes_AndRealCall(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	liveURL := startPlainMCPServer(t)

	appYAML := `schema_version: 2
app:
  app_id: mcptest
  name: MCP All Types
  version: "0.1.0"
  description: "Exercises every MCP server shape."
  author: "x@y.z"
  category: "coding"

agents:
  - id: main
    role: worker
    brain:
      provider: anthropic
      model: claude-sonnet-4-6
      config:
        api_key: "{{env.ANTHROPIC_API_KEY}}"
    system_prompt: "Use the MCP tools."
    modules:
      - mcp

tools:
  modules:
    mcp:
      config:
        servers:
          sequential_thinking: {}
          github:
            token: "ghp_example"
          postgres:
            connection_string: "postgresql://u:p@h:5432/db"
          hosted:
            via: smithery
            smithery_key: "sk-smithery"
          live:
            transport: streamable_http
            url: "` + liveURL + `"
            sandbox:
              permissions: [net.http]
  capabilities:
    default_policy: auto
    grant:
      - module: mcp
`

	dir := t.TempDir()
	appPath := filepath.Join(dir, "app.yaml")
	if err := os.WriteFile(appPath, []byte(appYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. Compile the real app YAML through the real compiler.
	comp := compiler.New().WithSources(compilercatalog.RegistrySource{Registry: pkgmodule.Default})
	res, err := comp.Compile(appPath)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !res.OK() {
		t.Fatalf("app did not compile cleanly: %v", res.Diagnostics.Errors())
	}

	// 2. Resolve every declared server through the real catalog/smithery path.
	block := res.Definition.Tools.Modules["mcp"]
	servers, _ := schema.NormalizeServers(block.Config["servers"])
	m := New()

	st, ok := m.resolveServer(context.Background(), "sequential_thinking", servers["sequential_thinking"], false)
	if !ok || st.Command != "npx" || st.Args[1] != "@modelcontextprotocol/server-sequential-thinking" {
		t.Fatalf("sequential_thinking (bare npm) resolution: %+v ok=%v", st, ok)
	}
	gh, ok := m.resolveServer(context.Background(), "github", servers["github"], false)
	if !ok || gh.Env["GITHUB_PERSONAL_ACCESS_TOKEN"] != "ghp_example" {
		t.Fatalf("github (shorthand) resolution: %+v ok=%v", gh, ok)
	}
	pg, ok := m.resolveServer(context.Background(), "postgres", servers["postgres"], false)
	if !ok || pg.Args[len(pg.Args)-1] != "postgresql://u:p@h:5432/db" {
		t.Fatalf("postgres (arg-append) resolution: %+v ok=%v", pg, ok)
	}
	sm, ok := m.resolveServer(context.Background(), "hosted", servers["hosted"], false)
	if !ok || sm.Transport != "streamable_http" || sm.Headers["Authorization"] != "Bearer sk-smithery" {
		t.Fatalf("hosted (smithery) resolution: %+v ok=%v", sm, ok)
	}
	lv, ok := m.resolveServer(context.Background(), "live", servers["live"], false)
	if !ok || lv.URL != liveURL {
		t.Fatalf("live (explicit http) resolution: %+v ok=%v", lv, ok)
	}

	// 3. The agent calls a tool on the live server, configured purely from YAML.
	live := New()
	t.Cleanup(func() { _ = live.Stop(context.Background()) })
	callCfg := map[string]any{"servers": map[string]any{
		"live": map[string]any{"transport": "streamable_http", "url": liveURL},
	}}
	ctx := tool.WithIdentity(context.Background(), tool.Identity{UserID: "u", ModuleID: "mcp"})
	ctx = pkgmodule.WithModuleConfig(ctx, callCfg)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	out, err := live.Invoke(ctx, "mcp_live__echo", []byte(`{"text":"from-yaml"}`))
	if err != nil {
		t.Fatalf("invoke transport error: %v", err)
	}
	if !out.Success {
		t.Fatalf("tool call on YAML-configured server failed: %+v", out)
	}
	data, _ := out.Data.(map[string]any)
	if s, _ := data["output"].(string); !strings.Contains(s, "echo:from-yaml") {
		t.Fatalf("unexpected tool output: %v", data["output"])
	}
}
