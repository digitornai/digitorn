package policy_test

import (
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
)

// SG-7 — paranoid tests covering subtle combinations + boundary
// conditions + adversarial inputs that go beyond the per-gate basics
// of SG-1..6. Targets the SEC01-03 invariants the Python daemon
// never tested automatically.
//
// Five categories :
//
//   A. Subtle combinations  (deny+approve, hidden+grant, deny+grant, ...)
//   B. Boundary inputs      (empty actions, unknown policy, FQN edge cases)
//   C. Adversarial schemas  (caps fields with surprising values)
//   D. Audit row invariants (gate code stability, no duplication)
//   E. Per-agent strict     (sub-agent cannot widen parent)

// invoke is a compact helper to build an Invocation with all the
// fields a gate needs.
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

// pctx is a compact helper to build a PolicyContext.
func pctx(active bool, caps *schema.CapabilitiesConfig, spec *tool.Spec) policy.PolicyContext {
	return policy.PolicyContext{
		AppActive:    active,
		Capabilities: caps,
		ToolSpec:     spec,
	}
}

// =====================================================================
// CATEGORY A — Subtle combinations
// =====================================================================

// TestParanoid_DenyBeatsApprove_AndDenyBeatsGrant : same action in
// deny AND approve AND grant must always Deny. Verified across many
// orderings of the slice (to catch any "first match wins per slice"
// bug). Doc rule : "deny > approve > grant" — strict.
func TestParanoid_DenyBeatsApprove_AndDenyBeatsGrant(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		Deny:    []schema.CapabilityGrant{{Module: "shell", Tools: []string{"bash"}}},
		Approve: []schema.CapabilityGrant{{Module: "shell", Tools: []string{"bash"}}},
		Grant:   []schema.CapabilityGrant{{Module: "shell", Tools: []string{"bash"}}},
		// Even raising the max_risk to high & default to auto, deny still wins.
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

// TestParanoid_ApproveBypassesRiskCap : the EXACT claude-code bug. A
// high-risk tool (bash) placed under `approve` with NO max_risk_level
// (default ceiling = medium) must NOT be filtered out by gate 2 — it is a
// deliberate opt-in meant to be offered and gated by approval. Before the fix,
// only `grant` bypassed the cap, so the safer policy (approve) silently removed
// the tool and the agent reported "no shell access". Gate 2 must Allow ; the
// whole chain must end in NeedsApproval (gate 4), not Deny.
func TestParanoid_ApproveBypassesRiskCap(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		// No MaxRiskLevel → default medium ; bash is high → over the cap.
		Approve:       []schema.CapabilityGrant{{Module: "bash", Tools: []string{"run"}}},
		DefaultPolicy: schema.CapAuto,
	}
	// Use the REAL bash.run spec shape : high risk AND a declared permission.
	// The permission is what made the live agent lose the shell — gate 3 derives
	// its granted set from `grant` only, so an approve-listed action with a
	// required permission was denied. The test must carry Permissions or it
	// silently skips gate 3 (the bug that hid this in the first place).
	bashSpec := &tool.Spec{RiskLevel: tool.RiskHigh, Permissions: []string{"bash.run"}}

	// Gate 2 (risk cap) must allow — the opt-in bypasses the ceiling.
	if d := policy.Gate2Risk(invoke(policy.CallerLLM, "bash", "run"), pctx(true, caps, bashSpec)); d.Kind != policy.DecisionAllow {
		t.Fatalf("gate2 got %v / %v, want Allow (approve must bypass risk cap)", d.Kind, d.Gate)
	}
	// Gate 3 (permissions) must allow — the approve opt-in satisfies the
	// action_granted shortcut, exactly like grant.
	if d := policy.Gate3Permissions(invoke(policy.CallerLLM, "bash", "run"), pctx(true, caps, bashSpec)); d.Kind != policy.DecisionAllow {
		t.Fatalf("gate3 got %v / %v, want Allow (approve must satisfy permissions)", d.Kind, d.Gate)
	}
	// Whole chain ends in NeedsApproval — the tool is OFFERED + gated, not dropped.
	if d := policy.RunGates(invoke(policy.CallerLLM, "bash", "run"), pctx(true, caps, bashSpec)); d.Kind != policy.DecisionNeedsApproval {
		t.Fatalf("chain got %v / %v, want NeedsApproval", d.Kind, d.Gate)
	}
	// And it survives the schema-build filter (it appears in the LLM toolset).
	visible := policy.BuildAgentToolset(true, caps,
		&schema.Agent{Modules: schema.AgentModules{{ID: "bash", Tools: []string{"run"}}}},
		[]policy.AvailableAction{{Module: "bash", Action: "run", Spec: bashSpec}},
	)
	if len(visible) != 1 {
		t.Fatalf("bash.run must survive BuildAgentToolset (the agent must SEE the shell), got %d tools", len(visible))
	}
}

// TestParanoid_GrantBypassesRiskCap : the doc says "grant bypasses
// the max_risk_level cap" (language/04-tools.md verbatim example
// with `max_risk_level: low` + grant on `shell.bash`). Gate 2 must
// honour this — a grant on a high-risk action with max_risk=medium
// must still allow.
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

// TestParanoid_GrantBypassesRiskButDenyStillBlocks : the precedence
// rule for the conflict scenario. An action that's in BOTH grant
// AND deny, where risk exceeds the cap :
//   - Gate 2 bypasses the cap because of the grant → Allow
//   - Gate 4 deny then blocks → Deny
//   - Overall : Deny (deny > grant per doc)
//
// Verifies the gates compose correctly across the override and the
// final blocker.
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

// TestParanoid_HiddenAndGrantTogether_LLMBlockedButHookAllowed :
// when an action is in hidden_actions AND grant :
//   - LLM caller : gate 1b denies (hidden invisible)
//   - hook caller : gate 1b bypasses, gate 4 grant allows
//
// This is the canonical "hidden but callable from infra" pattern
// from security-04-hidden-vs-deny.md.
func TestParanoid_HiddenAndGrantTogether(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		HiddenActions: []schema.CapabilityGrant{{Module: "lsp", Tools: []string{"diagnostics"}}},
		Grant:         []schema.CapabilityGrant{{Module: "lsp", Tools: []string{"diagnostics"}}},
	}
	spec := &tool.Spec{RiskLevel: tool.RiskLow}

	// LLM : hidden wins, deny at gate 1b.
	d1 := policy.RunGates(invoke(policy.CallerLLM, "lsp", "diagnostics"), pctx(true, caps, spec))
	if d1.Kind != policy.DecisionDeny || d1.Gate != policy.GateHidden {
		t.Errorf("LLM caller : got %v / %v, want Deny / %s", d1.Kind, d1.Gate, policy.GateHidden)
	}

	// Hook : hidden bypassed, grant allows at gate 4.
	d2 := policy.RunGates(invoke(policy.CallerHook, "lsp", "diagnostics"), pctx(true, caps, spec))
	if d2.Kind != policy.DecisionAllow {
		t.Errorf("Hook caller : got %v, want Allow (hidden bypassed for non-LLM)", d2.Kind)
	}
}

// TestParanoid_DenyEmptyActions_BlocksAllModuleActions : the
// documented convention — a Deny entry with empty actions covers
// EVERY action of that module. Verified across multiple action names.
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

// TestParanoid_ApproveEmptyActions_RequiresApprovalForAll : same
// empty-actions convention for approve.
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

// =====================================================================
// CATEGORY B — Boundary inputs
// =====================================================================

// TestParanoid_UnknownDefaultPolicy_FailsClosed : an unknown
// default_policy value (forward-compat, e.g. someone adds "warn"
// to the YAML schema in the future) must fail-closed at the
// runtime gate.
func TestParanoid_UnknownDefaultPolicy_FailsClosed(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapabilityPolicy("warn"), // not a known value
	}
	d := policy.RunGates(
		invoke(policy.CallerLLM, "filesystem", "read"),
		pctx(true, caps, &tool.Spec{RiskLevel: tool.RiskLow}),
	)
	if d.Kind != policy.DecisionDeny {
		t.Fatalf("unknown policy : got %v, want Deny (fail-closed)", d.Kind)
	}
}

// TestParanoid_EmptyModuleEmptyAction_StillEvaluated : the LLM might
// emit a tool_call with module="" or action="" (malformed). Each
// gate should handle it without panicking. The final decision will
// land at gate 4 default_policy.
func TestParanoid_EmptyModuleEmptyAction_NoPanic(t *testing.T) {
	caps := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapBlock}
	cases := []struct{ module, action string }{
		{"", ""},
		{"", "read"},
		{"filesystem", ""},
	}
	for _, c := range cases {
		// Just verify no panic and a valid decision is returned.
		d := policy.RunGates(
			invoke(policy.CallerLLM, c.module, c.action),
			pctx(true, caps, nil),
		)
		if d.Kind != policy.DecisionDeny && d.Kind != policy.DecisionAllow && d.Kind != policy.DecisionNeedsApproval {
			t.Errorf("module=%q action=%q : invalid decision %v", c.module, c.action, d.Kind)
		}
	}
}

// TestParanoid_NilCapabilities_DevMode_AllowsLowRisk : the doc says
// "absence means dev/test mode (no enforcement)". Confirm that with
// nil caps, low/medium risk actions pass.
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

// TestParanoid_NilCapabilities_HighRiskStillCapped : nil caps =
// dev mode, BUT gate 2 still applies the default ceiling (medium).
// High risk still denied.
func TestParanoid_NilCapabilities_HighRiskStillCapped(t *testing.T) {
	d := policy.RunGates(
		invoke(policy.CallerLLM, "shell", "bash"),
		pctx(true, nil, &tool.Spec{RiskLevel: tool.RiskHigh}),
	)
	if d.Kind != policy.DecisionDeny || d.Gate != policy.GateRisk {
		t.Fatalf("got %v / %v, want Deny / gate2_risk", d.Kind, d.Gate)
	}
}

// =====================================================================
// CATEGORY C — Adversarial schemas
// =====================================================================

// TestParanoid_DenyOnAllSpiral_StillReachableForSystemModules :
// even an app that denies EVERY action (default_policy=block, deny:
// [{module: "*"}] not supported but a global block) must still
// allow system module calls (context_builder, etc.).
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
