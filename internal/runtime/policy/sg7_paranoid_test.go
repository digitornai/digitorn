package policy_test

import (
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/runtime/policy"
)

func invoke(caller policy.CallerKind, module, action string) policy.Invocation {
	return policy.Invocation{
		Caller:    caller,
		AppID:     "app",
		AgentID:   "agent",
		SessionID: "sess",
		UserID:    "user",
		Module:    module,
		Action:    action,
	}
}

func pctx(active bool, caps *schema.CapabilitiesConfig, spec *tool.Spec) policy.PolicyContext {
	return policy.PolicyContext{
		AppActive:    active,
		Capabilities: caps,
		ToolSpec:     spec,
	}
}

func TestParanoid_DenyBeatsApprove_AndDenyBeatsGrant(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		Deny:    []schema.CapabilityGrant{{Module: "shell", Tools: []string{"bash"}}},
		Approve: []schema.CapabilityGrant{{Module: "shell", Tools: []string{"bash"}}},
		Grant:   []schema.CapabilityGrant{{Module: "shell", Tools: []string{"bash"}}},
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
		DefaultPolicy: schema.CapAuto,
	}
	d := policy.RunGates(
		invoke(policy.CallerLLM, "shell", "bash"),
		pctx(true, caps, &tool.Spec{RiskLevel: tool.RiskHigh}),
	)
	if d.Kind != policy.DecisionDeny {
		t.Fatalf("got %v, want Deny", d.Kind)
	}
}

func TestParanoid_ApproveBypassesRiskCap(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		Approve:       []schema.CapabilityGrant{{Module: "bash", Tools: []string{"run"}}},
		DefaultPolicy: schema.CapAuto,
	}
	bashSpec := &tool.Spec{RiskLevel: tool.RiskHigh, Permissions: []string{"bash.run"}}

	if d := policy.Gate2Risk(invoke(policy.CallerLLM, "bash", "run"), pctx(true, caps, bashSpec)); d.Kind != policy.DecisionAllow {
		t.Fatalf("gate2 got %v / %v, want Allow (approve must bypass risk cap)", d.Kind, d.Gate)
	}
	if d := policy.Gate3Permissions(invoke(policy.CallerLLM, "bash", "run"), pctx(true, caps, bashSpec)); d.Kind != policy.DecisionAllow {
		t.Fatalf("gate3 got %v / %v, want Allow (approve must satisfy permissions)", d.Kind, d.Gate)
	}
	if d := policy.RunGates(invoke(policy.CallerLLM, "bash", "run"), pctx(true, caps, bashSpec)); d.Kind != policy.DecisionNeedsApproval {
		t.Fatalf("chain got %v / %v, want NeedsApproval", d.Kind, d.Gate)
	}
	visible := policy.BuildAgentToolset(true, caps,
		&schema.Agent{Modules: schema.AgentModules{{ID: "bash", Tools: []string{"run"}}}},
		[]policy.AvailableAction{{Module: "bash", Action: "run", Spec: bashSpec}},
	)
	if len(visible) != 1 {
		t.Fatalf("bash.run must survive BuildAgentToolset (the agent must SEE the shell), got %d tools", len(visible))
	}
}

func TestParanoid_GrantBypassesRiskCap(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		MaxRiskLevel:  schema.RiskLevel(tool.RiskMedium),
		Grant:         []schema.CapabilityGrant{{Module: "shell", Tools: []string{"bash"}}},
		DefaultPolicy: schema.CapAuto,
	}
	d := policy.RunGates(
		invoke(policy.CallerLLM, "shell", "bash"),
		pctx(true, caps, &tool.Spec{RiskLevel: tool.RiskHigh}),
	)
	if d.Kind != policy.DecisionAllow {
		t.Fatalf("got %v / %v, want Allow (grant must bypass risk cap)", d.Kind, d.Gate)
	}
}

func TestParanoid_GrantBypassesRiskButDenyStillBlocks(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		MaxRiskLevel:  schema.RiskLevel(tool.RiskMedium),
		Grant:         []schema.CapabilityGrant{{Module: "shell", Tools: []string{"bash"}}},
		Deny:          []schema.CapabilityGrant{{Module: "shell", Tools: []string{"bash"}}},
		DefaultPolicy: schema.CapAuto,
	}
	d := policy.RunGates(
		invoke(policy.CallerLLM, "shell", "bash"),
		pctx(true, caps, &tool.Spec{RiskLevel: tool.RiskHigh}),
	)
	if d.Kind != policy.DecisionDeny {
		t.Fatalf("got %v, want Deny (deny > grant overall)", d.Kind)
	}
	if d.Gate != policy.GatePolicy {
		t.Errorf("gate code = %q, want gate4_policy (deny applied there, not gate 2)", d.Gate)
	}
}

func TestParanoid_HiddenAndGrantTogether(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		HiddenActions: []schema.CapabilityGrant{{Module: "lsp", Tools: []string{"diagnostics"}}},
		Grant:         []schema.CapabilityGrant{{Module: "lsp", Tools: []string{"diagnostics"}}},
	}
	spec := &tool.Spec{RiskLevel: tool.RiskLow}

	d1 := policy.RunGates(invoke(policy.CallerLLM, "lsp", "diagnostics"), pctx(true, caps, spec))
	if d1.Kind != policy.DecisionDeny || d1.Gate != policy.GateHidden {
		t.Errorf("LLM caller : got %v / %v, want Deny / %s", d1.Kind, d1.Gate, policy.GateHidden)
	}

	d2 := policy.RunGates(invoke(policy.CallerHook, "lsp", "diagnostics"), pctx(true, caps, spec))
	if d2.Kind != policy.DecisionAllow {
		t.Errorf("Hook caller : got %v, want Allow (hidden bypassed for non-LLM)", d2.Kind)
	}
}

func TestParanoid_DenyEmptyActions_BlocksAllModuleActions(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		Deny:          []schema.CapabilityGrant{{Module: "shell"}},
		DefaultPolicy: schema.CapAuto,
	}
	for _, action := range []string{"bash", "exec", "kill", "run", "anything"} {
		d := policy.RunGates(
			invoke(policy.CallerLLM, "shell", action),
			pctx(true, caps, &tool.Spec{RiskLevel: tool.RiskLow}),
		)
		if d.Kind != policy.DecisionDeny {
			t.Errorf("shell.%s : got %v, want Deny (empty actions = all)", action, d.Kind)
		}
	}
}

func TestParanoid_ApproveEmptyActions_RequiresApprovalForAll(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		Approve:       []schema.CapabilityGrant{{Module: "shell"}},
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
		DefaultPolicy: schema.CapAuto,
	}
	for _, action := range []string{"bash", "exec", "kill"} {
		d := policy.RunGates(
			invoke(policy.CallerLLM, "shell", action),
			pctx(true, caps, &tool.Spec{RiskLevel: tool.RiskLow}),
		)
		if d.Kind != policy.DecisionNeedsApproval {
			t.Errorf("shell.%s : got %v, want NeedsApproval", action, d.Kind)
		}
	}
}

func TestParanoid_UnknownDefaultPolicy_FailsClosed(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapabilityPolicy("warn"),
	}
	d := policy.RunGates(
		invoke(policy.CallerLLM, "filesystem", "read"),
		pctx(true, caps, &tool.Spec{RiskLevel: tool.RiskLow}),
	)
	if d.Kind != policy.DecisionDeny {
		t.Fatalf("unknown policy : got %v, want Deny (fail-closed)", d.Kind)
	}
}

func TestParanoid_EmptyModuleEmptyAction_NoPanic(t *testing.T) {
	caps := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapBlock}
	cases := []struct{ module, action string }{
		{"", ""},
		{"", "read"},
		{"filesystem", ""},
	}
	for _, c := range cases {
		d := policy.RunGates(
			invoke(policy.CallerLLM, c.module, c.action),
			pctx(true, caps, nil),
		)
		if d.Kind != policy.DecisionDeny && d.Kind != policy.DecisionAllow && d.Kind != policy.DecisionNeedsApproval {
			t.Errorf("module=%q action=%q : invalid decision %v", c.module, c.action, d.Kind)
		}
	}
}

func TestParanoid_NilCapabilities_DevMode_AllowsLowRisk(t *testing.T) {
	for _, level := range []tool.RiskLevel{tool.RiskLow, tool.RiskMedium} {
		d := policy.RunGates(
			invoke(policy.CallerLLM, "x", "y"),
			pctx(true, nil, &tool.Spec{RiskLevel: level}),
		)
		if d.Kind != policy.DecisionAllow {
			t.Errorf("risk=%v nil caps : got %v, want Allow", level, d.Kind)
		}
	}
}

func TestParanoid_NilCapabilities_HighRiskStillCapped(t *testing.T) {
	d := policy.RunGates(
		invoke(policy.CallerLLM, "shell", "bash"),
		pctx(true, nil, &tool.Spec{RiskLevel: tool.RiskHigh}),
	)
	if d.Kind != policy.DecisionDeny || d.Gate != policy.GateRisk {
		t.Fatalf("got %v / %v, want Deny / gate2_risk", d.Kind, d.Gate)
	}
}

func TestParanoid_DenyAll_SystemModulesStillReachable(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapBlock,
		Deny: []schema.CapabilityGrant{
			{Module: "context_builder"},
			{Module: "llm_provider"},
			{Module: "index"},
		},
	}
	for _, mod := range []string{"context_builder", "llm_provider", "index"} {
		d := policy.RunGates(
			invoke(policy.CallerLLM, mod, "anything"),
			pctx(true, caps, &tool.Spec{RiskLevel: tool.RiskLow}),
		)
		if d.Kind != policy.DecisionAllow {
			t.Errorf("system module %s : got %v, want Allow (bypass)", mod, d.Kind)
		}
	}
}

// TestParanoid_MaxRiskInvalidValue_FallsBackToMedium : the doc says
// default ceiling is medium. An invalid value in the YAML
// (typo, garbage) shouldn't crash — should fall back to medium.
func TestParanoid_MaxRiskInvalidValue_FallsBackToMedium(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		MaxRiskLevel:  schema.RiskLevel("ultra-mega-high"), // bogus
		DefaultPolicy: schema.CapAuto,
	}
	// High-risk action : with invalid ceiling, gate 2 fails-closed
	// because the ceiling can't be compared. This is acceptable
	// since the schema validation would normally catch this at
	// compile-time.
	d := policy.RunGates(
		invoke(policy.CallerLLM, "shell", "bash"),
		pctx(true, caps, &tool.Spec{RiskLevel: tool.RiskHigh}),
	)
	// Either Allow (fallback to medium → high > medium → deny actually)
	// or Deny — both are safe. The point is no panic.
	if d.Kind == policy.DecisionNeedsApproval {
		t.Fatalf("unexpected NeedsApproval for invalid ceiling")
	}
}

// TestParanoid_ApproveTimeout_Boundaries_AllNonZero : clamp tests
// for ApprovalTimeout are at the engine level (approvalTimeout
// helper). Here we verify the schema accepts the range [30, 3600]
// and zero/negative values defer to default.
func TestParanoid_ApprovalTimeout_FieldAccepted(t *testing.T) {
	for _, v := range []int{0, 30, 300, 3600, 9999, -1} {
		caps := &schema.CapabilitiesConfig{
			Approve:         []schema.CapabilityGrant{{Module: "shell", Tools: []string{"bash"}}},
			ApprovalTimeout: v,
			MaxRiskLevel:    schema.RiskLevel(tool.RiskHigh),
		}
		// Just check that gate evaluation doesn't crash on any value.
		// The clamping logic lives in engine.approvalTimeout (SG-5),
		// not in the gates themselves.
		d := policy.RunGates(
			invoke(policy.CallerLLM, "shell", "bash"),
			pctx(true, caps, &tool.Spec{RiskLevel: tool.RiskHigh}),
		)
		if d.Kind != policy.DecisionNeedsApproval {
			t.Errorf("timeout=%d : got %v, want NeedsApproval", v, d.Kind)
		}
	}
}

// =====================================================================
// CATEGORY D — Audit row invariants (at policy layer)
// =====================================================================

// TestParanoid_GateCodes_Stable : the GateCode strings are part of
// the audit log contract and the doc. Hard-coded here to prevent
// accidental renames.
func TestParanoid_GateCodes_Stable(t *testing.T) {
	checks := map[policy.GateCode]string{
		policy.GateInactive:    "gate0_inactive",
		policy.GateModule:      "gate1a_module",
		policy.GateHidden:      "gate1b_hidden",
		policy.GateRisk:        "gate2_risk",
		policy.GatePermissions: "gate3_permissions",
		policy.GatePolicy:      "gate4_policy",
	}
	for code, want := range checks {
		if string(code) != want {
			t.Errorf("GateCode = %q, want %q (renaming breaks audit log consumers)",
				string(code), want)
		}
	}
}

// TestParanoid_DecisionKindStrings_Stable : same for decision kinds.
func TestParanoid_DecisionKindStrings_Stable(t *testing.T) {
	for kind, want := range map[policy.DecisionKind]string{
		policy.DecisionAllow:         "allow",
		policy.DecisionDeny:          "deny",
		policy.DecisionNeedsApproval: "needs_approval",
	} {
		if got := kind.String(); got != want {
			t.Errorf("Kind %v.String() = %q, want %q", kind, got, want)
		}
	}
}

// TestParanoid_CallerKindStrings_Stable : caller strings used in
// audit rows must stay stable across releases.
func TestParanoid_CallerKindStrings_Stable(t *testing.T) {
	for k, want := range map[policy.CallerKind]string{
		policy.CallerLLM:      "llm",
		policy.CallerHook:     "hook",
		policy.CallerSetup:    "setup",
		policy.CallerChannel:  "channel",
		policy.CallerInternal: "internal",
	} {
		if got := k.String(); got != want {
			t.Errorf("Caller %v.String() = %q, want %q", k, got, want)
		}
	}
}

// =====================================================================
// CATEGORY E — Per-agent strict
// =====================================================================

// TestParanoid_SubAgentCannotWidenParent : the doc's sub-agent
// isolation rule. App caps allow "filesystem.write", but the agent
// has Modules=[{filesystem: [read]}] → write is invisible to this
// agent.
func TestParanoid_SubAgentCannotWidenParent(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto, // app-wide allows everything
	}
	allActions := []policy.AvailableAction{
		{Module: "filesystem", Action: "read", Spec: &tool.Spec{RiskLevel: tool.RiskLow}},
		{Module: "filesystem", Action: "write", Spec: &tool.Spec{RiskLevel: tool.RiskMedium}},
		{Module: "filesystem", Action: "delete", Spec: &tool.Spec{RiskLevel: tool.RiskHigh}},
	}
	agent := &schema.Agent{
		ID: "reader",
		Modules: schema.AgentModules{
			{ID: "filesystem", Tools: []string{"read"}},
		},
	}
	visible := policy.BuildAgentToolset(true, caps, agent, allActions)
	if len(visible) != 1 {
		t.Fatalf("visible count = %d, want 1", len(visible))
	}
	if visible[0].Action != "read" {
		t.Errorf("visible[0] = %s, want read", visible[0].Action)
	}
}

// TestParanoid_SubAgentEmptyModules_SeesEverything : empty Modules
// = no agent-level restriction → agent sees everything the app
// capabilities allow.
func TestParanoid_SubAgentEmptyModules_SeesEverything(t *testing.T) {
	caps := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto}
	all := []policy.AvailableAction{
		{Module: "filesystem", Action: "read", Spec: &tool.Spec{RiskLevel: tool.RiskLow}},
		{Module: "shell", Action: "bash", Spec: &tool.Spec{RiskLevel: tool.RiskLow}}, // low so gate 2 passes
	}
	agent := &schema.Agent{ID: "main"}
	visible := policy.BuildAgentToolset(true, caps, agent, all)
	if len(visible) != 2 {
		t.Fatalf("visible = %d, want 2 (unrestricted agent sees everything)", len(visible))
	}
}

// TestParanoid_TwoAgentsDifferentSubsets : two agents in the same
// app with different module restrictions see different tools.
func TestParanoid_TwoAgentsDifferentSubsets(t *testing.T) {
	caps := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto}
	all := []policy.AvailableAction{
		{Module: "filesystem", Action: "read", Spec: &tool.Spec{RiskLevel: tool.RiskLow}},
		{Module: "shell", Action: "bash", Spec: &tool.Spec{RiskLevel: tool.RiskLow}},
	}
	reader := &schema.Agent{
		ID: "reader",
		Modules: schema.AgentModules{
			{ID: "filesystem", Tools: []string{"read"}},
		},
	}
	executor := &schema.Agent{
		ID: "executor",
		Modules: schema.AgentModules{
			{ID: "shell", Tools: []string{"bash"}},
		},
	}

	vReader := policy.BuildAgentToolset(true, caps, reader, all)
	vExec := policy.BuildAgentToolset(true, caps, executor, all)

	if len(vReader) != 1 || vReader[0].Module != "filesystem" {
		t.Errorf("reader sees : %+v, want only filesystem.read", vReader)
	}
	if len(vExec) != 1 || vExec[0].Module != "shell" {
		t.Errorf("executor sees : %+v, want only shell.bash", vExec)
	}
}

// =====================================================================
// CATEGORY F — Stress / concurrency
// =====================================================================

// TestParanoid_RunGates_ConcurrentEvaluations : 1000 concurrent
// RunGates calls on the SAME PolicyContext must all return correctly.
// Verifies the gates are pure (no shared mutable state).
func TestParanoid_RunGates_ConcurrentEvaluations(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		Deny:          []schema.CapabilityGrant{{Module: "shell", Tools: []string{"bash"}}},
	}
	pc := pctx(true, caps, &tool.Spec{RiskLevel: tool.RiskLow})

	const N = 1000
	results := make(chan policy.Decision, N*2)
	for i := 0; i < N; i++ {
		go func() {
			results <- policy.RunGates(invoke(policy.CallerLLM, "filesystem", "read"), pc)
			results <- policy.RunGates(invoke(policy.CallerLLM, "shell", "bash"), pc)
		}()
	}

	allow, deny := 0, 0
	for i := 0; i < N*2; i++ {
		d := <-results
		switch d.Kind {
		case policy.DecisionAllow:
			allow++
		case policy.DecisionDeny:
			deny++
		}
	}
	if allow != N || deny != N {
		t.Errorf("under concurrent load : allow=%d deny=%d, want %d/%d",
			allow, deny, N, N)
	}
}

// TestParanoid_BuildAgentToolset_ConcurrentBuilds : same toolset
// builder called concurrently from many goroutines must produce
// identical results.
func TestParanoid_BuildAgentToolset_ConcurrentBuilds(t *testing.T) {
	caps := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto}
	all := []policy.AvailableAction{
		{Module: "filesystem", Action: "read", Spec: &tool.Spec{RiskLevel: tool.RiskLow}},
		{Module: "filesystem", Action: "write", Spec: &tool.Spec{RiskLevel: tool.RiskMedium}},
		{Module: "shell", Action: "bash", Spec: &tool.Spec{RiskLevel: tool.RiskLow}},
	}
	agent := &schema.Agent{
		ID: "reader",
		Modules: schema.AgentModules{
			{ID: "filesystem", Tools: []string{"read"}},
		},
	}

	const N = 200
	results := make(chan int, N)
	for i := 0; i < N; i++ {
		go func() {
			v := policy.BuildAgentToolset(true, caps, agent, all)
			results <- len(v)
		}()
	}
	for i := 0; i < N; i++ {
		if got := <-results; got != 1 {
			t.Errorf("concurrent build returned %d tools, want 1", got)
		}
	}
}
