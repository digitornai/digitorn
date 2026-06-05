package policy_test

import (
	"sort"
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
)

// ---- helpers -------------------------------------------------------

// catalog is a thin test-only builder for AvailableAction lists.
// Each entry is a "<module>.<action>:<risk>" string ; the helper
// turns it into AvailableAction with a tool.Spec.
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

// fqns extracts the (module.action) strings from a result, sorted —
// makes test assertions stable across slice reordering.
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

// ---- documented YAML scenarios -------------------------------------

// TestBuild_HiddenBot : reproduces hidden-bot.yaml from
// security-04-hidden-vs-deny.md. filesystem.glob is in hidden_actions ;
// it must not appear in the LLM's toolset. The other filesystem
// actions stay.
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

// TestBuild_DenyBot : reproduces deny-bot.yaml. filesystem.glob is
// in deny — gate 4 blocks it. From the LLM's perspective the result
// is identical to hidden-bot : glob is absent.
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

// TestBuild_GatesBot : reproduces gates-bot.yaml from
// security-02-gates.md gate 2 live demo. shell.bash is risk_level:high
// while max_risk_level: medium → bash must be filtered at schema-build
// time. The doc captures the live agent reply : "I don't have a
// shell.bash tool available."
func TestBuild_GatesBot(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskMedium),
		Grant: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"read", "glob", "grep"}},
		},
	}
	allActions := catalog(
		e("shell", "bash", tool.RiskHigh), // filtered by gate 2
		e("filesystem", "read", tool.RiskLow),
		e("filesystem", "write", tool.RiskMedium),
		e("filesystem", "glob", tool.RiskLow),
		e("filesystem", "grep", tool.RiskLow),
	)
	agent := &schema.Agent{ID: "main"}

	visible := policy.BuildAgentToolset(true, caps, agent, allActions)
	// shell.bash filtered (gate 2). filesystem.write is not in grant
	// but default_policy=auto so it passes (gate 4 allow).
	assertVisible(t, visible,
		"filesystem.read",
		"filesystem.write",
		"filesystem.glob",
		"filesystem.grep",
	)
}

// TestBuild_SubAgentIsolation : reproduces the canonical
// advanced-01-sub-agent-isolation.md pattern. The "reader" sub-agent
// declares modules: [{filesystem: [read, glob, grep]}, memory]. It
// must see only those actions even when the app has full filesystem
// access.
func TestBuild_SubAgentIsolation(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto, // app-wide allows everything
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
			{ID: "memory"}, // bare = all actions
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

// TestBuild_ApprovalBot : reproduces approval-bot.yaml from
// security-01-approval.md. shell.bash is in approve. The doc shows
// the LLM CAN see bash in its toolset (it issues the tool call,
// the approval pause fires at runtime). Confirm BuildAgentToolset
// keeps it in the list.
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
	// approve = NeedsApproval = kept in list (the LLM will see it
	// and try to call it ; the pause happens at runtime).
	assertVisible(t, visible, "shell.bash")
}

// ---- inactive app + edge cases -------------------------------------

// TestBuild_InactiveApp_EmptyToolset : gate 0 denies for every
// action → result is empty regardless of capabilities.
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

// TestBuild_NilCapabilities_AllVisible : security.md says "absence
// means dev/test mode (no enforcement)" — without capabilities,
// every available action passes (subject to risk_level default
// medium and agent modules subset).
func TestBuild_NilCapabilities_AllLowAndMediumVisible(t *testing.T) {
	allActions := catalog(
		e("filesystem", "read", tool.RiskLow),
		e("filesystem", "write", tool.RiskMedium),
		e("shell", "bash", tool.RiskHigh), // default ceiling medium → filtered
	)
	agent := &schema.Agent{ID: "main"}

	visible := policy.BuildAgentToolset(true, nil, agent, allActions)
	// Without caps : gate 2 still applies its default ceiling
	// (medium per the doc). shell.bash (high) gets filtered.
	assertVisible(t, visible, "filesystem.read", "filesystem.write")
}

// TestBuild_PermissionsGated : an action declares required_permissions
// that none of the grants match → filtered at gate 3. With NO grants
// configured, the derived granted_permissions set is empty so no
// symbolic permission match is possible → deny.
//
// Mirrors the Python daemon's behaviour where granted_permissions is
// derived from capabilities.grant[].actions as "module:action" strings
// (security.py + compiler.py audit) — there is no YAML surface for
// declaring agent-level permissions.
func TestBuild_PermissionsGated(t *testing.T) {
	caps := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto}
	allActions := catalog(
		ep("filesystem", "write", tool.RiskMedium, "fs.write"),
		ep("filesystem", "read", tool.RiskLow, "fs.read"),
	)
	agent := &schema.Agent{ID: "main"}

	visible := policy.BuildAgentToolset(true, caps, agent, allActions)
	// Without any grants, gate 3 denies every action declaring
	// required_permissions — neither read nor write is visible.
	assertVisible(t, visible /* none */)
}

// TestBuild_HiddenAndDeny_BothEffectiveAndIdempotent : the
// "defence in depth" pattern from security-04-hidden-vs-deny.md.
// Listing the same action in both deny and hidden_actions is
// redundant but should produce identical visible-output. Test that
// it doesn't crash or double-filter.
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

	// max_risk_level default is medium so risk=high would also exclude
	// delete on its own. Bump to high to isolate hidden+deny behaviour.
	caps.MaxRiskLevel = schema.RiskLevel(tool.RiskHigh)

	visible := policy.BuildAgentToolset(true, caps, agent, allActions)
	assertVisible(t, visible, "filesystem.read")
}

// TestBuild_HiddenModules_WholeModuleHidden : hidden_modules removes
// every action of the module from the LLM's view.
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

// TestBuild_PreservesOrder : the LLM's tool list order matches the
// input catalog order. Stable iteration is required for deterministic
// system-prompt assembly (downstream toolregistry / toolplanner).
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

// TestBuild_EmptyInput_EmptyOutput : trivial guard.
func TestBuild_EmptyInput_EmptyOutput(t *testing.T) {
	visible := policy.BuildAgentToolset(true, nil, &schema.Agent{}, nil)
	if len(visible) != 0 {
		t.Fatalf("empty input should yield empty output, got %d", len(visible))
	}
}

// ---- ResolveAgentModules unit tests --------------------------------

// TestResolveAgentModules_EmptyIsNil : the documented signal for
// "no restriction" is nil — distinct from "modules: []" (which is
// "no modules allowed" but doesn't currently occur in practice).
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

// TestResolveAgentModules_BareNameAllActions : `modules: [shell]`
// gives AllActions=true.
func TestResolveAgentModules_BareNameAllActions(t *testing.T) {
	got := policy.ResolveAgentModules(schema.AgentModules{
		{ID: "shell"},
	})
	if got["shell"].AllActions != true {
		t.Fatalf("expected AllActions=true, got %+v", got["shell"])
	}
}

// TestResolveAgentModules_ActionSubset : `modules:
// [{filesystem: [read, glob]}]` gives Actions={read,glob}.
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

// TestResolveAgentModules_MultiEntrySameModule_Merges : if a YAML
// declares the same module twice (legal in AgentModules), the
// actions merge.
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
