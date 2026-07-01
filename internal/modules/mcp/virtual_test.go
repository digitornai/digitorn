package mcp

import (
	"context"
	"testing"

	"github.com/digitornai/digitorn/internal/domain/tool"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestInferRisk(t *testing.T) {
	cases := []struct {
		name string
		risk tool.RiskLevel
		irr  bool
	}{
		{"delete_repo", tool.RiskHigh, true},
		{"drop_table", tool.RiskHigh, true},
		{"list_issues", tool.RiskLow, false},
		{"get_user", tool.RiskLow, false},
		{"create_issue", tool.RiskMedium, false},
		{"post_message", tool.RiskMedium, false},
	}
	for _, c := range cases {
		r, irr := inferRisk(c.name)
		if r != c.risk || irr != c.irr {
			t.Errorf("%s → (%v,%v), want (%v,%v)", c.name, r, irr, c.risk, c.irr)
		}
	}
}

func TestSchemaToParams(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":  map[string]any{"type": "string", "description": "a file"},
			"count": map[string]any{"type": "integer"},
		},
		"required": []any{"path"},
	}
	params := schemaToParams(schema)
	by := map[string]tool.ParamSpec{}
	for _, p := range params {
		by[p.Name] = p
	}
	if p := by["path"]; !p.Path || !p.Required || p.Type != "string" {
		t.Errorf("path param wrong: %+v", p)
	}
	if p := by["count"]; p.Path || p.Required || p.Type != "integer" {
		t.Errorf("count param wrong: %+v", p)
	}
}

func TestLiveToolsMaterialization(t *testing.T) {
	fc := &fakeConn{
		tools:     toolList("delete_repo", "list_issues"),
		prompts:   []*mcpsdk.Prompt{{Name: "review"}},
		resources: []*mcpsdk.Resource{{URI: "file://readme"}},
	}
	m := New()
	m.pool.dialFn = func(context.Context, connectSpec) (mcpConn, error) { return fc, nil }
	if _, err := m.pool.connect(context.Background(), "srv", connectSpec{}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	specs := m.LiveTools(context.Background())
	by := map[string]tool.Spec{}
	for _, s := range specs {
		if s.RiskLevel == "" {
			t.Fatalf("tool %q has empty RiskLevel — gate 2 would fail closed", s.Name)
		}
		by[s.Name] = s
	}

	if s, ok := by["mcp_srv__delete_repo"]; !ok || s.RiskLevel != tool.RiskHigh || !s.Irreversible {
		t.Errorf("delete_repo spec wrong: %+v", s)
	}
	if s, ok := by["mcp_srv__list_issues"]; !ok || s.RiskLevel != tool.RiskLow {
		t.Errorf("list_issues spec wrong: %+v", s)
	}
	if _, ok := by["mcp_srv__get_prompt"]; !ok {
		t.Error("prompt tools must be materialized when the server has prompts")
	}
	r, ok := by["mcp_srv__read_resource"]
	if !ok {
		t.Fatal("resource tools must be materialized when the server has resources")
	}
	for _, p := range r.Params {
		if p.Name == "uri" && p.Path {
			t.Error("resource uri must NOT be Path:true (server-namespaced, not a workspace path)")
		}
	}
}
