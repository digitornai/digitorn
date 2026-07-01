package injection

import (
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime/context/index"
	"github.com/digitornai/digitorn/internal/runtime/policy"
)

func TestMcpWireAlias(t *testing.T) {
	cases := []struct {
		fqn, alias, canon string
		ok                bool
	}{
		{"mcp_everything.echo", "echo", "mcp_everything.echo", true},
		{"mcp_everything__echo", "echo", "mcp_everything.echo", true}, // underscore FQN form too
		{"mcp_sequential_thinking.sequentialthinking", "sequentialthinking", "mcp_sequential_thinking.sequentialthinking", true},
		{"filesystem.read", "", "", false}, // native: not an MCP tool
		{"mcp_x", "", "", false},           // no action part
	}
	for _, c := range cases {
		a, canon, ok := mcpWireAlias(c.fqn)
		if a != c.alias || canon != c.canon || ok != c.ok {
			t.Errorf("mcpWireAlias(%q) = (%q,%q,%v), want (%q,%q,%v)", c.fqn, a, canon, ok, c.alias, c.canon, c.ok)
		}
	}
}

// disambiguateMCPAliases must keep every wire Name unique, never rename a
// native/builtin tool, and qualify a colliding MCP alias with its server.
func TestDisambiguateMCPAliases(t *testing.T) {
	specs := []llm.ToolSpec{
		{Name: "read"},                          // native — fixed, owns the name
		{Name: "echo", Canonical: "mcp_a.echo"}, // two servers expose echo
		{Name: "echo", Canonical: "mcp_b.echo"},
		{Name: "read", Canonical: "mcp_c.read"}, // MCP tool shadowing the native read
	}
	disambiguateMCPAliases(specs)

	if specs[0].Name != "read" {
		t.Errorf("native read must never be renamed, got %q", specs[0].Name)
	}
	if specs[3].Name == "read" {
		t.Errorf("MCP read must yield to the native read, got %q", specs[3].Name)
	}
	seen := map[string]int{}
	for _, s := range specs {
		seen[s.Name]++
	}
	for n, c := range seen {
		if c > 1 {
			t.Errorf("wire name %q is not unique (%d occurrences)", n, c)
		}
	}
}

// End to end through the planner's schema builder: an MCP tool ships the bare
// tool name to the LLM, carries its canonical FQN, and the native tool keeps
// the standard underscored wire form.
func TestBuildDirectSchemas_MCPGetsBareWireName(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	universe := []policy.AvailableAction{
		{Module: "mcp_everything", Action: "echo", Spec: &tool.Spec{Name: "mcp_everything.echo", Description: "Echo back a message.", RiskLevel: tool.RiskLow}},
		{Module: "filesystem", Action: "read", Spec: &tool.Spec{Name: "filesystem.read", Description: "Read a file.", RiskLevel: tool.RiskLow}},
	}
	idx := index.NewBuilder().Build(true, caps, &schema.Agent{ID: "main"}, universe)

	specs := buildDirectSchemas(idx)
	var mcp, fs *llm.ToolSpec
	for i := range specs {
		if specs[i].Canonical == "mcp_everything.echo" {
			mcp = &specs[i]
		}
		if specs[i].Name == "filesystem__read" {
			fs = &specs[i]
		}
	}
	if mcp == nil {
		t.Fatal("MCP tool missing from direct schemas")
	}
	if mcp.Name != "echo" {
		t.Errorf("MCP wire name = %q, want bare %q", mcp.Name, "echo")
	}
	if fs == nil {
		t.Error("native filesystem.read must keep wire name filesystem__read")
	}
}
