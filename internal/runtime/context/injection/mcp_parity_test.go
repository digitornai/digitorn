package injection

import (
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime/context/index"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
)

// TestBuildDirectSchemas_MCPParity proves an MCP tool's DESCRIPTION and full
// PARAMETER SCHEMA reach the LLM identically to a native digitorn tool: the same
// builder, the same llm.ToolSpec fields, the same JSON-schema shape. The only
// difference is the wire name (bare for MCP, underscored for native). This is the
// guarantee that "an MCP tool is seen by the agent exactly like a native tool".
func TestBuildDirectSchemas_MCPParity(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	mcpSpec := &tool.Spec{
		Name:        "mcp_demo.search",
		Description: "Search the demo corpus and return ranked hits.",
		Params: []tool.ParamSpec{
			{Name: "query", Type: "string", Description: "Full-text query.", Required: true},
			{Name: "limit", Type: "integer", Description: "Max hits to return."},
		},
		RiskLevel: tool.RiskLow,
	}
	nativeSpec := &tool.Spec{
		Name:        "filesystem.grep",
		Description: "Search files for a pattern and return matches.",
		Params: []tool.ParamSpec{
			{Name: "pattern", Type: "string", Description: "Regex to search for.", Required: true},
			{Name: "path", Type: "string", Description: "Root path to search under."},
		},
		RiskLevel: tool.RiskLow,
	}
	universe := []policy.AvailableAction{
		{Module: "mcp_demo", Action: "search", Spec: mcpSpec},
		{Module: "filesystem", Action: "grep", Spec: nativeSpec},
	}
	idx := index.NewBuilder().Build(true, caps, &schema.Agent{ID: "main"}, universe)

	specs := buildDirectSchemas(idx)
	var mcp, native *llm.ToolSpec
	for i := range specs {
		switch {
		case specs[i].Canonical == "mcp_demo.search":
			mcp = &specs[i]
		case specs[i].Name == "filesystem__grep":
			native = &specs[i]
		}
	}
	if mcp == nil || native == nil {
		t.Fatalf("missing specs: mcp=%v native=%v", mcp != nil, native != nil)
	}

	// (1) Each tool carries its OWN description, verbatim — MCP no differently.
	if mcp.Description != mcpSpec.Description {
		t.Errorf("MCP description not injected verbatim: got %q want %q", mcp.Description, mcpSpec.Description)
	}
	if native.Description != nativeSpec.Description {
		t.Errorf("native description: got %q want %q", native.Description, nativeSpec.Description)
	}

	// (2) Each tool's parameters are a fully-formed JSON schema, same shape.
	assertParamSchema(t, "mcp", mcp, []string{"query", "limit"}, "query")
	assertParamSchema(t, "native", native, []string{"pattern", "path"}, "pattern")

	// (3) The ONLY difference is the wire name + the canonical alias.
	if mcp.Name != "search" {
		t.Errorf("MCP wire name = %q, want bare %q", mcp.Name, "search")
	}
	if native.Name != "filesystem__grep" {
		t.Errorf("native wire name = %q, want %q", native.Name, "filesystem__grep")
	}
	if native.Canonical != "" {
		t.Errorf("native tool must carry no canonical alias, got %q", native.Canonical)
	}
}

func assertParamSchema(t *testing.T, label string, s *llm.ToolSpec, wantProps []string, wantRequired string) {
	t.Helper()
	if s.Parameters == nil {
		t.Fatalf("%s: parameters not injected (nil)", label)
	}
	if s.Parameters["type"] != "object" {
		t.Errorf("%s: schema type = %v, want object", label, s.Parameters["type"])
	}
	props, ok := s.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s: properties not a JSON object: %T", label, s.Parameters["properties"])
	}
	for _, p := range wantProps {
		pm, ok := props[p].(map[string]any)
		if !ok {
			t.Errorf("%s: property %q missing from schema", label, p)
			continue
		}
		if d, _ := pm["description"].(string); d == "" {
			t.Errorf("%s: property %q injected without its description", label, p)
		}
	}
	if !requiredContains(s.Parameters["required"], wantRequired) {
		t.Errorf("%s: required = %v, want to contain %q", label, s.Parameters["required"], wantRequired)
	}
}

func requiredContains(v any, want string) bool {
	switch r := v.(type) {
	case []string:
		for _, x := range r {
			if x == want {
				return true
			}
		}
	case []any:
		for _, x := range r {
			if s, _ := x.(string); s == want {
				return true
			}
		}
	}
	return false
}
