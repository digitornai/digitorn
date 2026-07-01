package server

import (
	"context"
	"testing"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/pkg/module"
)

type fakeMCPModule struct {
	*fakeModule
	live []tool.Spec
}

func (f *fakeMCPModule) LiveTools(context.Context) []tool.Spec { return f.live }

func mcpAppMgr(appID string) *fakeAppMgr {
	return &fakeAppMgr{apps: map[string]*appmgr.RuntimeApp{
		appID: {
			Meta: &appmgr.App{AppID: appID, Enabled: true},
			Definition: &schema.AppDefinition{
				App: schema.AppMeta{AppID: appID, Name: appID, Version: "1.0"},
				Tools: &schema.ToolsBlock{Modules: map[string]schema.ModuleBlock{
					"mcp": {Config: map[string]any{"servers": map[string]any{}}},
				}},
			},
		},
	}}
}

func mcpRegistry(t *testing.T, live []tool.Spec) *module.Registry {
	t.Helper()
	reg := module.NewRegistry()
	fm := &fakeMCPModule{fakeModule: newFakeModule("mcp"), live: live}
	if err := reg.Register(func() domainmodule.Module { return fm }); err != nil {
		t.Fatalf("register mcp: %v", err)
	}
	if err := reg.Start(context.Background(), "mcp"); err != nil {
		t.Fatalf("start mcp: %v", err)
	}
	return reg
}

func TestMCPCatalog_MaterializesNativeActions(t *testing.T) {
	live := []tool.Spec{
		{Name: "mcp_github__create_issue", RiskLevel: tool.RiskHigh, Tags: []string{"mcp", "github"}},
		{Name: "mcp_github__list_issues", RiskLevel: tool.RiskLow},
		{Name: "mcp_google_calendar__list_events", RiskLevel: tool.RiskLow},
	}
	mc := newMCPCatalog(mcpRegistry(t, live), mcpAppMgr("app1"), nil)

	actions := mc.forApp("app1")
	got := map[string]tool.RiskLevel{}
	for _, a := range actions {
		if a.Spec == nil {
			t.Fatalf("action %s/%s has nil spec", a.Module, a.Action)
		}
		got[a.Module+"|"+a.Action+"|"+a.Spec.Name] = a.Spec.RiskLevel
	}

	want := map[string]tool.RiskLevel{
		"mcp_github|create_issue|mcp_github.create_issue":                 tool.RiskHigh,
		"mcp_github|list_issues|mcp_github.list_issues":                   tool.RiskLow,
		"mcp_google_calendar|list_events|mcp_google_calendar.list_events": tool.RiskLow,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d actions, want %d: %v", len(got), len(want), got)
	}
	for k, risk := range want {
		if got[k] != risk {
			t.Errorf("%s: risk %q, want %q", k, got[k], risk)
		}
	}
}

func TestMCPCatalog_LookupSpecForGates(t *testing.T) {
	live := []tool.Spec{{Name: "mcp_github__delete_repo", RiskLevel: tool.RiskHigh, Irreversible: true}}
	mc := newMCPCatalog(mcpRegistry(t, live), mcpAppMgr("app1"), nil)
	mc.forApp("app1") // populate the global spec cache

	spec := mc.lookupSpec("mcp_github", "delete_repo")
	if spec == nil {
		t.Fatal("lookupSpec must resolve an MCP tool spec (else gates fail closed)")
	}
	if spec.RiskLevel != tool.RiskHigh || !spec.Irreversible {
		t.Errorf("spec wrong: %+v", spec)
	}
	if mc.lookupSpec("mcp_github", "ghost") != nil {
		t.Error("unknown action must return nil")
	}
}

func TestMCPCatalog_NoMCPDeclared(t *testing.T) {
	live := []tool.Spec{{Name: "mcp_github__list", RiskLevel: tool.RiskLow}}
	// app2 declares no mcp module → no MCP actions.
	mc := newMCPCatalog(mcpRegistry(t, live), &fakeAppMgr{apps: map[string]*appmgr.RuntimeApp{
		"app2": appWithModules("app2", "filesystem"),
	}}, nil)
	if a := mc.forApp("app2"); len(a) != 0 {
		t.Fatalf("app without mcp must get no MCP actions, got %v", a)
	}
}
