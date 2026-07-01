//go:build mcpintegration

package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/pkg/module"
)

// TestMCPToolDescriptions_InjectedVerbatim connects the REAL everything server
// and proves that every MCP tool's DESCRIPTION (and every declared input
// property) is injected straight from the server — verbatim, not invented or
// placeholdered — the same source-of-truth a native tool gets from its own
// registration. This is the guarantee the agent reads the server's real docs.
//
//	go test -tags mcpintegration -run TestMCPToolDescriptions_InjectedVerbatim ./internal/modules/mcp/ -v
func TestMCPToolDescriptions_InjectedVerbatim(t *testing.T) {
	ctx := module.WithModuleConfig(context.Background(), map[string]any{
		"servers": map[string]any{"ev": map[string]any{
			"transport": "stdio", "command": "npx",
			"args": []any{"-y", "@modelcontextprotocol/server-everything"},
		}},
	})
	m := New()
	defer m.pool.shutdown(context.Background())

	specs := m.LiveTools(ctx)
	if len(specs) == 0 {
		t.Skip("everything server unreachable (npx/network) — skipping")
	}
	byName := make(map[string]tool.Spec, len(specs))
	for _, s := range specs {
		byName[s.Name] = s
	}

	checked, withParams := 0, 0
	for _, srv := range m.pool.live() {
		for _, raw := range srv.tools {
			if raw == nil {
				continue
			}
			spec, ok := byName["mcp_"+srv.id+"__"+raw.Name]
			if !ok {
				t.Errorf("server tool %q was not materialized as a native spec", raw.Name)
				continue
			}
			// Description: verbatim from the server (when the server gives one).
			if rawDesc := strings.TrimSpace(raw.Description); rawDesc != "" && spec.Description != rawDesc {
				t.Errorf("%s: description not injected verbatim\n server: %q\n spec:   %q", raw.Name, rawDesc, spec.Description)
			} else if spec.Description == "" {
				t.Errorf("%s: empty description reached the agent", raw.Name)
			}
			// Schema: every server-declared input property is exposed as a param,
			// parsed here independently of the module's own converter.
			props := schemaPropNames(raw.InputSchema)
			for _, p := range props {
				if !hasParam(spec.Params, p) {
					t.Errorf("%s: declared input property %q not injected as a param", raw.Name, p)
				}
			}
			if len(props) > 0 {
				withParams++
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no server tools were checked")
	}
	t.Logf("verbatim injection verified for %d real MCP tools (%d with parameter schemas) from `everything`", checked, withParams)
}

func schemaPropNames(input any) []string {
	if input == nil {
		return nil
	}
	b, err := json.Marshal(input)
	if err != nil {
		return nil
	}
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	out := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		out = append(out, k)
	}
	return out
}

func hasParam(ps []tool.ParamSpec, name string) bool {
	for _, p := range ps {
		if p.Name == name {
			return true
		}
	}
	return false
}
