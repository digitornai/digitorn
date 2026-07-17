package policy_test

import (
	"sort"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/runtime/policy"
)

func catalog(entries ...catEntry) []policy.AvailableAction {
	out := make([]policy.AvailableAction, len(entries))
	for i, e := range entries {
		out[i] = policy.AvailableAction{
			Module: e.module,
			Action: e.action,
			Spec: &tool.Spec{
				Name:        e.module + "." + e.action,
				RiskLevel:   e.risk,
				Permissions: e.perms,
			},
		}
	}
	return out
}

type catEntry struct {
	module string
	action string
	risk   tool.RiskLevel
	perms  []string
}

func e(module, action string, risk tool.RiskLevel) catEntry {
	return catEntry{module: module, action: action, risk: risk}
}
func ep(module, action string, risk tool.RiskLevel, perms ...string) catEntry {
	return catEntry{module: module, action: action, risk: risk, perms: perms}
}

func fqns(result []policy.AvailableAction) []string {
	out := make([]string, len(result))
	for i, a := range result {
		out[i] = a.Module + "." + a.Action
	}
	sort.Strings(out)
	return out
}

func assertVisible(t *testing.T, result []policy.AvailableAction, want ...string) {
	t.Helper()
	got := fqns(result)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("toolset size : got %d %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("toolset[%d] = %q, want %q\n  got=%v\n  want=%v", i, got[i], want[i], got, want)
		}
	}
}

func TestBuild_HiddenBot(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		HiddenActions: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"glob"}},
		},
	}
	allActions := catalog(
		e("filesystem", "read", tool.RiskLow),
		e("filesystem", "write", tool.RiskMedium),
		e("filesystem", "edit", tool.RiskMedium),
		e("filesystem", "glob", tool.RiskLow),
		e("filesystem", "grep", tool.RiskLow),
	)
	agent := &schema.Agent{ID: "main"}

	visible := policy.BuildAgentToolset(true, caps, agent, allActions)
	assertVisible(t, visible,
		"filesystem.read",
		"filesystem.write",
		"filesystem.edit",
		"filesystem.grep",
	)
}

func TestBuild_DenyBot(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		Deny: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"glob"}},
		},
	}
	allActions := catalog(
		e("filesystem", "read", tool.RiskLow),
		e("filesystem", "write", tool.RiskMedium),
		e("filesystem", "edit", tool.RiskMedium),
		e("filesystem", "glob", tool.RiskLow),
		e("filesystem", "grep", tool.RiskLow),
	)
	agent := &schema.Agent{ID: "main"}

	visible := policy.BuildAgentToolset(true, caps, agent, allActions)
	assertVisible(t, visible,
		"filesystem.read",
		"filesystem.write",
		"filesystem.edit",
		"filesystem.grep",
	)
}

func TestBuild_GatesBot(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskMedium),
		Grant: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"read", "glob", "grep"}},
		},
	}
	allActions := catalog(
		e("shell", "bash", tool.RiskHigh),
		e("filesystem", "read", tool.RiskLow),
		e("filesystem", "write", tool.RiskMedium),
		e("filesystem", "glob", tool.RiskLow),
		e("filesystem", "grep", tool.RiskLow),
	)
	agent := &schema.Agent{ID: "main"}

	visible := policy.BuildAgentToolset(true, caps, agent, allActions)
	assertVisible(t, visible,
		"filesystem.read",
		"filesystem.write",
		"filesystem.glob",
		"filesystem.grep",
	)
}

func TestBuild_SubAgentIsolation(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
	}
	allActions := catalog(
		e("filesystem", "read", tool.RiskLow),
		e("filesystem", "write", tool.RiskMedium),
		e("filesystem", "edit", tool.RiskMedium),
		e("filesystem", "glob", tool.RiskLow),
		e("filesystem", "grep", tool.RiskLow),
		e("memory", "store", tool.RiskLow),
		e("memory", "recall", tool.RiskLow),
		e("shell", "bash", tool.RiskMedium),
	)
	agent := &schema.Agent{
		ID: "reader",
		Modules: schema.AgentModules{
			{ID: "filesystem", Tools: []string{"read", "glob", "grep"}},
			{ID: "memory"},
		},
	}

	visible := policy.BuildAgentToolset(true, caps, agent, allActions)
	assertVisible(t, visible,
		"filesystem.read",
		"filesystem.glob",
		"filesystem.grep",
		"memory.store",
		"memory.recall",
	)
}

func TestBuild_ApprovalBot(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
		Approve: []schema.CapabilityGrant{
			{Module: "shell", Tools: []string{"bash"},
				Reason: "Shell commands need explicit approval before running."},
		},
		ApprovalTimeout: 60,
	}
	allActions := catalog(
		e("shell", "bash", tool.RiskHigh),
	)
	agent := &schema.Agent{ID: "main"}

	visible := policy.BuildAgentToolset(true, caps, agent, allActions)
	assertVisible(t, visible, "shell.bash")
}

func TestBuild_InactiveApp_EmptyToolset(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		Grant: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"read"}},
		},
	}
	allActions := catalog(
		e("filesystem", "read", tool.RiskLow),
		e("filesystem", "write", tool.RiskMedium),
	)
	agent := &schema.Agent{ID: "main"}

	visible := policy.BuildAgentToolset(false, caps, agent, allActions)
	if len(visible) != 0 {
		t.Fatalf("inactive app should yield empty toolset, got %v", fqns(visible))
	}
}

func TestBuild_NilCapabilities_AllLowAndMediumVisible(t *testing.T) {
	allActions := catalog(
		e("filesystem", "read", tool.RiskLow),
		e("filesystem", "write", tool.RiskMedium),
		e("shell", "bash", tool.RiskHigh),
	)
	agent := &schema.Agent{ID: "main"}

	visible := policy.BuildAgentToolset(true, nil, agent, allActions)
	assertVisible(t, visible, "filesystem.read", "filesystem.write")
}

func TestBuild_PermissionsGated(t *testing.T) {
	caps := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto}
	allActions := catalog(
		ep("filesystem", "write", tool.RiskMedium, "fs.write"),
		ep("filesystem", "read", tool.RiskLow, "fs.read"),
	)
	agent := &schema.Agent{ID: "main"}

	visible := policy.BuildAgentToolset(true, caps, agent, allActions)
	assertVisible(t, visible)
}

func TestBuild_HiddenAndDeny_BothEffectiveAndIdempotent(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		Deny: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"delete"}},
		},
		HiddenActions: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"delete"}},
		},
	}
	allActions := catalog(
		e("filesystem", "read", tool.RiskLow),
		e("filesystem", "delete", tool.RiskHigh),
	)
	agent := &schema.Agent{ID: "main"}

	caps.MaxRiskLevel = schema.RiskLevel(tool.RiskHigh)

	visible := policy.BuildAgentToolset(true, caps, agent, allActions)
	assertVisible(t, visible, "filesystem.read")
}

func TestBuild_HiddenModules_WholeModuleHidden(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
		HiddenModules: []string{"shell"},
	}
	allActions := catalog(
		e("shell", "bash", tool.RiskHigh),
		e("shell", "ps", tool.RiskLow),
		e("filesystem", "read", tool.RiskLow),
	)
	agent := &schema.Agent{ID: "main"}

	visible := policy.BuildAgentToolset(true, caps, agent, allActions)
	assertVisible(t, visible, "filesystem.read")
}

func TestBuild_PreservesOrder(t *testing.T) {
	caps := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto}
	allActions := catalog(
		e("z", "first", tool.RiskLow),
		e("a", "second", tool.RiskLow),
		e("m", "third", tool.RiskLow),
	)
	agent := &schema.Agent{ID: "main"}

	visible := policy.BuildAgentToolset(true, caps, agent, allActions)
	if len(visible) != 3 {
		t.Fatalf("len=%d, want 3", len(visible))
	}
	if visible[0].Module != "z" || visible[1].Module != "a" || visible[2].Module != "m" {
		t.Fatalf("order broken : %v", fqns(visible))
	}

}

func TestBuild_EmptyInput_EmptyOutput(t *testing.T) {
	visible := policy.BuildAgentToolset(true, nil, &schema.Agent{}, nil)
	if len(visible) != 0 {
		t.Fatalf("empty input should yield empty output, got %d", len(visible))
	}
}

func TestResolveAgentModules_EmptyIsNil(t *testing.T) {
	got := policy.ResolveAgentModules(nil)
	if got != nil {
		t.Fatalf("nil input should yield nil, got %v", got)
	}
	got = policy.ResolveAgentModules(schema.AgentModules{})
	if got != nil {
		t.Fatalf("empty input should yield nil, got %v", got)
	}
}

func TestResolveAgentModules_BareNameAllActions(t *testing.T) {
	got := policy.ResolveAgentModules(schema.AgentModules{
		{ID: "shell"},
	})
	if got["shell"].AllActions != true {
		t.Fatalf("expected AllActions=true, got %+v", got["shell"])
	}
}

func TestResolveAgentModules_ActionSubset(t *testing.T) {
	got := policy.ResolveAgentModules(schema.AgentModules{
		{ID: "filesystem", Tools: []string{"read", "glob"}},
	})
	access := got["filesystem"]
	if access.AllActions {
		t.Fatalf("AllActions should be false")
	}
	if _, ok := access.Actions["read"]; !ok {
		t.Errorf("missing 'read'")
	}
	if _, ok := access.Actions["glob"]; !ok {
		t.Errorf("missing 'glob'")
	}
	if _, ok := access.Actions["write"]; ok {
		t.Errorf("'write' should not be allowed")
	}
}

func TestCanAgentCall_MCPUmbrellaGrantsVirtualModules(t *testing.T) {
	mcp := policy.PolicyContext{AgentModules: policy.ResolveAgentModules(schema.AgentModules{{ID: "mcp"}})}
	if !mcp.CanAgentCall("mcp_everything", "echo") {
		t.Error("declaring `mcp` must grant mcp_everything.echo")
	}
	if !mcp.CanAgentCall("mcp_sequential_thinking", "sequentialthinking") {
		t.Error("declaring `mcp` must grant any mcp_<server> tool")
	}

	subset := policy.PolicyContext{AgentModules: policy.ResolveAgentModules(schema.AgentModules{{ID: "mcp", Tools: []string{"echo"}}})}
	if !subset.CanAgentCall("mcp_everything", "echo") {
		t.Error("`mcp: [echo]` must grant mcp_everything.echo")
	}
	if subset.CanAgentCall("mcp_everything", "add") {
		t.Error("`mcp: [echo]` must NOT grant mcp_everything.add")
	}

	noMCP := policy.PolicyContext{AgentModules: policy.ResolveAgentModules(schema.AgentModules{{ID: "filesystem"}})}
	if noMCP.CanAgentCall("mcp_everything", "echo") {
		t.Error("agent without `mcp` must NOT reach mcp_everything")
	}
	if noMCP.CanAgentCall("shell", "run") {
		t.Error("undeclared non-MCP module must stay denied")
	}

	if !(policy.PolicyContext{}).CanAgentCall("mcp_everything", "echo") {
		t.Error("nil AgentModules must allow")
	}
}

func TestResolveAgentModules_MultiEntrySameModule_Merges(t *testing.T) {
	got := policy.ResolveAgentModules(schema.AgentModules{
		{ID: "filesystem", Tools: []string{"read"}},
		{ID: "filesystem", Tools: []string{"glob", "grep"}},
	})
	access := got["filesystem"]
	if access.AllActions {
		t.Fatalf("AllActions should be false")
	}
	for _, a := range []string{"read", "glob", "grep"} {
		if _, ok := access.Actions[a]; !ok {
			t.Errorf("missing %q after merge", a)
		}
	}
}
