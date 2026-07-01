//go:build mcpintegration

package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/pkg/module"
)

// TestModuleE2E proves the module's full runtime path against a real server:
// config-per-call → lazy connect → LiveTools materialization + Invoke routing +
// the untrusted-output envelope. Gated (needs npx + network):
//
//	go test -tags mcpintegration -run TestModuleE2E ./internal/modules/mcp/ -v
func TestModuleE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	cfg := map[string]any{"servers": map[string]any{
		"everything": map[string]any{
			"transport": "stdio",
			"command":   "npx",
			"args":      []any{"-y", "@modelcontextprotocol/server-everything"},
			"sandbox":   map[string]any{"permissions": []any{"process.exec"}},
		},
	}}
	ctx = module.WithModuleConfig(ctx, cfg)

	m := New()
	defer m.pool.shutdown(context.Background())

	specs := m.LiveTools(ctx)
	if len(specs) == 0 {
		t.Fatal("LiveTools materialized nothing — lazy connect or discovery failed")
	}
	var echo *tool.Spec
	for i := range specs {
		if specs[i].RiskLevel == "" {
			t.Fatalf("tool %q has empty RiskLevel (gate 2 would fail closed)", specs[i].Name)
		}
		if specs[i].Name == "mcp_everything__echo" {
			echo = &specs[i]
		}
	}
	if echo == nil {
		t.Fatalf("expected mcp_everything__echo among %d materialized tools", len(specs))
	}
	t.Logf("materialized %d MCP tools as native specs", len(specs))

	res, err := m.Invoke(ctx, "mcp_everything__echo", []byte(`{"message":"digitorn-e2e"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Success {
		t.Fatalf("echo failed: %+v", res)
	}
	data, _ := res.Data.(map[string]any)
	if data["_note"] != injectionNote {
		t.Error("missing injection-defense note on a real tool result")
	}
	if out, _ := data["output"].(string); !strings.Contains(out, "digitorn-e2e") {
		t.Fatalf("echo output missing payload: %q", out)
	}
}
