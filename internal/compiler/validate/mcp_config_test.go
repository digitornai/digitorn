package validate

import (
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/diagnostic"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

func runMCP(cfg, cons map[string]any) *diagnostic.Bag {
	bag := diagnostic.NewBag()
	def := &schema.AppDefinition{
		Tools: &schema.ToolsBlock{
			Modules: map[string]schema.ModuleBlock{
				"mcp": {Config: cfg, Constraints: cons},
			},
		},
	}
	CheckMCPConfig("app.yaml", def, bag)
	return bag
}

func mcpSandbox() map[string]any {
	return map[string]any{"permissions": []any{"process.exec", "fs.read"}}
}

func TestCheckMCPConfigValid(t *testing.T) {
	bag := runMCP(map[string]any{
		"servers": map[string]any{
			"memory": map[string]any{},
			"custom": map[string]any{"transport": "stdio", "command": "/bin/x", "timeout": 30, "sandbox": mcpSandbox()},
			"remote": map[string]any{"transport": "streamable_http", "url": "https://x.example.com/mcp", "sandbox": map[string]any{"permissions": []any{"net.http"}}},
		},
	}, map[string]any{"allowed_servers": []any{"memory", "custom", "remote"}})
	if n := len(bag.Errors()); n != 0 {
		t.Fatalf("valid config produced %d errors: %v", n, bag.Errors())
	}
}

func TestCheckMCPConfigErrors(t *testing.T) {
	srv := func(m map[string]any) map[string]any { return map[string]any{"servers": map[string]any{"custom": m}} }
	cases := []struct {
		name string
		cfg  map[string]any
		cons map[string]any
		code diagnostic.Code
	}{
		{"bad_transport", srv(map[string]any{"transport": "ftp", "command": "x", "sandbox": mcpSandbox()}), nil, diagnostic.CodeBadEnum},
		{"stdio_missing_command", srv(map[string]any{"transport": "stdio", "sandbox": mcpSandbox()}), nil, diagnostic.CodeMissingRequired},
		{"http_missing_url", srv(map[string]any{"transport": "http", "sandbox": mcpSandbox()}), nil, diagnostic.CodeMissingRequired},
		{"inline_no_sandbox", srv(map[string]any{"transport": "stdio", "command": "x"}), nil, diagnostic.CodeMissingRequired},
		{"bad_server_id", map[string]any{"servers": map[string]any{"Bad_ID": map[string]any{}}}, nil, diagnostic.CodeBadRegex},
		{"unknown_top_field", map[string]any{"servers": map[string]any{}, "bogus": 1}, nil, diagnostic.CodeUnknownField},
		{"timeout_range", srv(map[string]any{"transport": "stdio", "command": "x", "timeout": 500, "sandbox": mcpSandbox()}), nil, diagnostic.CodeOutOfRange},
		{"sandbox_unknown_field", srv(map[string]any{"transport": "stdio", "command": "x", "sandbox": map[string]any{"permissions": []any{"process.exec"}, "bogus": 1}}), nil, diagnostic.CodeUnknownField},
		{"bad_permission", srv(map[string]any{"transport": "stdio", "command": "x", "sandbox": map[string]any{"permissions": []any{"wat"}}}), nil, diagnostic.CodeBadEnum},
		{"unknown_allowed_server", map[string]any{"servers": map[string]any{"memory": map[string]any{}}}, map[string]any{"allowed_servers": []any{"ghost"}}, diagnostic.CodeUnknownMCPServer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bag := runMCP(tc.cfg, tc.cons)
			if len(bag.ByCode(tc.code)) == 0 {
				t.Fatalf("expected code %s, got: %v", tc.code, bag.Errors())
			}
		})
	}
}
