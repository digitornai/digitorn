//go:build mcpintegration

package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/pkg/module"
)

// TestAutoConfig_RealServer_E2E is the ultra-real proof: a user references a
// server by BARE id that is NOT in the static catalog. The daemon auto-resolves
// it from the LIVE MCP registry, connects to the REAL remote server, materializes
// its REAL tools, and invokes one for a REAL result — no mocks, no hand-written
// command/url. Skips gracefully if the external server is unreachable.
//
//	go test -tags mcpintegration -run TestAutoConfig_RealServer_E2E ./internal/modules/mcp/ -v
func TestAutoConfig_RealServer_E2E(t *testing.T) {
	// `calculator` is unknown to the static catalog → forces tier-5 registry
	// resolution → com.mcpcalc/calculator (streamable_http, no auth).
	ctx := module.WithModuleConfig(context.Background(), map[string]any{
		"servers": map[string]any{"calculator": map[string]any{}},
	})
	m := New()
	defer m.pool.shutdown(context.Background())

	specs := m.LiveTools(ctx) // registry resolve + real connect, all from a bare id
	if len(specs) == 0 {
		t.Skip("registry/remote server unreachable — auto-config path not exercised live")
	}

	// Auto-config worked end to end: real tools materialized as native specs.
	var names []string
	for i := range specs {
		names = append(names, specs[i].Name)
	}
	t.Logf("auto-configured `calculator` from the live registry → %d real tools: %s",
		len(specs), strings.Join(names, ", "))

	if !hasTool(specs, "mcp_calculator__calculate") && !hasTool(specs, "mcp_calculator__list_calculators") {
		t.Fatalf("expected the real calculator tools, got: %v", names)
	}

	// Invoke a real tool on the real server.
	tool := "mcp_calculator__list_calculators"
	if !hasTool(specs, tool) {
		tool = "mcp_calculator__" + strings.TrimPrefix(names[0], "mcp_calculator__")
	}
	res, err := m.Invoke(ctx, tool, []byte(`{}`))
	if err != nil {
		t.Fatalf("%s invoke error: %v", tool, err)
	}
	data, _ := res.Data.(map[string]any)
	if data["_source"] != "mcp_server:calculator" {
		t.Errorf("result must carry the real server source, got %v", data["_source"])
	}
	t.Logf("invoked %s on the real auto-configured server → status=%v source=%v",
		tool, data["status"], data["_source"])
}
