package policy_test

import (
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
)

// ---- bypass cases (system modules + meta-tools) --------------------

// TestRunGates_SystemModule_BypassesEverything : a call to a system
// module never reaches the gates. The doc treats this as a security
// invariant ("trusted infrastructure") — even an explicit deny on a
// system module should not block. We verify with a deny capability
// that WOULD block any other module : the system module still passes.
func TestRunGates_SystemModule_BypassesEverything(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapBlock, // everything else denied
		Deny: []schema.CapabilityGrant{
			{Module: "context_builder"}, // even an explicit deny
		},
	}
	for _, mod := range []string{"context_builder", "llm_provider", "index"} {
		inv := policy.Invocation{Caller: policy.CallerLLM, Module: mod, Action: "anything"}
		pc := policy.PolicyContext{AppActive: true, Capabilities: caps}
		d := policy.RunGates(inv, pc)
		if d.Kind != policy.DecisionAllow {
			t.Errorf("module %q : got %v, want Allow (system bypass)", mod, d.Kind)
		}
		if d.Gate != "system_module_bypass" {
			t.Errorf("module %q : gate code = %q, want system_module_bypass", mod, d.Gate)
		}
	}
}

// TestRunGates_RuntimeInternalModule_BypassesGates : memory.* and
// agent_spawn.agent are dispatcher-intercepted runtime subsystems with NO bus
// spec. Without the bypass, gate2_risk fails closed ("tool spec unavailable")
// — the exact live failure that errored multi-agent delegation. They must
// short-circuit to Allow with the runtime_internal_bypass code, even when no
// spec lookup is wired (Lookup nil) and default_policy=block.
func TestRunGates_RuntimeInternalModule_BypassesGates(t *testing.T) {
	caps := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapBlock}
	cases := []struct{ module, action string }{
		{"agent_spawn", "agent"},
		{"memory", "set_goal"},
		{"memory", "remember"},
		{"memory", "task_create"},
		{"memory", "task_update"},
	}
	for _, c := range cases {
		inv := policy.Invocation{Caller: policy.CallerLLM, Module: c.module, Action: c.action}
		pc := policy.PolicyContext{AppActive: true, Capabilities: caps} // no Lookup → no spec
		d := policy.RunGates(inv, pc)
		if d.Kind != policy.DecisionAllow {
			t.Errorf("%s.%s : got %v (%s), want Allow (runtime-internal bypass)", c.module, c.action, d.Kind, d.Gate)
		}
		if d.Gate != "runtime_internal_bypass" {
			t.Errorf("%s.%s : gate code = %q, want runtime_internal_bypass", c.module, c.action, d.Gate)
		}
	}
}

// TestRunGates_MetaTool_BypassesEverything : the 10 builtin meta-
// actions (5 discovery meta-tools + 5 always-direct primitives) bypass
// the gates regardless of module, even under default_policy=block.
func TestRunGates_MetaTool_BypassesEverything(t *testing.T) {
	caps := &schema.CapabilitiesConfig{DefaultPolicy: schema.CapBlock}
	metaActions := []string{
		"search_tools", "get_tool", "execute_tool",
		"list_categories", "browse_category", "run_parallel",
		"background_run", "use_skill", "call_app", "ask_user",
	}
	for _, action := range metaActions {
		inv := policy.Invocation{Caller: policy.CallerLLM, Module: "anything", Action: action}
		pc := policy.PolicyContext{AppActive: true, Capabilities: caps}
		d := policy.RunGates(inv, pc)
		if d.Kind != policy.DecisionAllow {
			t.Errorf("action %q : got %v, want Allow (meta-tool bypass)", action, d.Kind)
		}
		if d.Gate != "meta_tool_bypass" {
			t.Errorf("action %q : gate code = %q, want meta_tool_bypass", action, d.Gate)
		}
	}
}

// TestRunGates_NormalActionGoesThroughChain : a non-system, non-meta
// action runs the full chain. Verify by injecting a deny on a low-
// risk action (so gate 2 passes and we land at gate 4).
func TestRunGates_NormalActionGoesThroughChain(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		Deny: []schema.CapabilityGrant{{Module: "filesystem", Tools: []string{"delete"}}},
	}
	inv := policy.Invocation{Caller: policy.CallerLLM, Module: "filesystem", Action: "delete"}
	pc := policy.PolicyContext{
		AppActive:    true,
		Capabilities: caps,
		ToolSpec:     &tool.Spec{RiskLevel: tool.RiskLow}, // passes gate 2
	}
	d := policy.RunGates(inv, pc)
	if d.Kind != policy.DecisionDeny {
		t.Fatalf("got %v, want Deny", d.Kind)
	}
	if d.Gate != policy.GatePolicy {
		t.Errorf("gate code = %q, want %q", d.Gate, policy.GatePolicy)
	}
}

// ---- gate ordering : earlier gate's Deny wins over later gates ----

// TestRunGates_Gate0InactiveStopsImmediately : when the app is
// inactive, gate 0 denies first — later gates never run. Verified
// by configuring capabilities that would deny at gate 4 with a
// different reason : the test asserts the FIRST gate (0) wins.
func TestRunGates_Gate0InactiveStopsImmediately(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		Deny: []schema.CapabilityGrant{{Module: "filesystem"}},
	}
	inv := policy.Invocation{Caller: policy.CallerLLM, Module: "filesystem", Action: "read"}
	pc := policy.PolicyContext{
		AppActive:    false, // gate 0 will deny here
		Capabilities: caps,
		ToolSpec:     &tool.Spec{RiskLevel: tool.RiskLow},
	}
	d := policy.RunGates(inv, pc)
	if d.Kind != policy.DecisionDeny {
		t.Fatalf("got %v, want Deny", d.Kind)
	}
	if d.Gate != policy.GateInactive {
		t.Errorf("gate code = %q, want %q (gate 0 should win over gate 4)",
			d.Gate, policy.GateInactive)
	}
}

// TestRunGates_Gate1bHidden_StopsBeforeGate2 : hidden is gate 1b,
// risk_level is gate 2. A hidden high-risk action should deny at
// gate 1b, not gate 2.
func TestRunGates_Gate1bHidden_StopsBeforeGate2(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		MaxRiskLevel: schema.RiskLevel(tool.RiskLow), // would also block via gate 2
		HiddenActions: []schema.CapabilityGrant{
			{Module: "shell", Tools: []string{"bash"}},
		},
	}
	inv := policy.Invocation{Caller: policy.CallerLLM, Module: "shell", Action: "bash"}
	pc := policy.PolicyContext{
		AppActive:    true,
		Capabilities: caps,
		ToolSpec:     &tool.Spec{RiskLevel: tool.RiskHigh},
	}
	d := policy.RunGates(inv, pc)
	if d.Kind != policy.DecisionDeny {
		t.Fatalf("got %v, want Deny", d.Kind)
	}
	if d.Gate != policy.GateHidden {
		t.Errorf("gate code = %q, want %q (1b should fire before 2)",
			d.Gate, policy.GateHidden)
	}
}

// TestRunGates_AllAllow_FinalIsLastGate : when every gate Allows, the
// returned Decision's Gate field is the LAST gate's code. With gate 5
// (classification) now in the chain, an unclassified action passes it
// cleanly and the final allow carries gate5_classification. Useful for
// the audit row (SG-6) to be specific.
func TestRunGates_AllAllow_FinalIsLastGate(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
	}
	inv := policy.Invocation{Caller: policy.CallerLLM, Module: "filesystem", Action: "read"}
	pc := policy.PolicyContext{
		AppActive:    true,
		Capabilities: caps,
		ToolSpec:     &tool.Spec{RiskLevel: tool.RiskLow},
	}
	d := policy.RunGates(inv, pc)
	if d.Kind != policy.DecisionAllow {
		t.Fatalf("got %v, want Allow", d.Kind)
	}
	if d.Gate != policy.GateClassification {
		t.Errorf("gate code = %q, want %q (final gate's code)",
			d.Gate, policy.GateClassification)
	}
}

// TestRunGates_NeedsApprovalReturnsImmediately : gate 4's
// NeedsApproval is blocking (the loop should not continue) — verify
// the orchestrator returns it as the final decision.
func TestRunGates_NeedsApprovalReturnsImmediately(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		Approve: []schema.CapabilityGrant{
			{Module: "shell", Tools: []string{"bash"},
				Reason: "shell needs approval"},
		},
	}
	inv := policy.Invocation{Caller: policy.CallerLLM, Module: "shell", Action: "bash"}
	pc := policy.PolicyContext{
		AppActive:    true,
		Capabilities: caps,
		ToolSpec:     &tool.Spec{RiskLevel: tool.RiskHigh}, // not capped (HighCeiling not set)
	}
	caps.MaxRiskLevel = schema.RiskLevel(tool.RiskHigh) // raise ceiling above 'high'
	d := policy.RunGates(inv, pc)
	if d.Kind != policy.DecisionNeedsApproval {
		t.Fatalf("got %v, want NeedsApproval", d.Kind)
	}
	if d.Reason != "shell needs approval" {
		t.Errorf("reason = %q, want 'shell needs approval'", d.Reason)
	}
}

// ---- IsSystemModule / IsMetaTool tests (public surface) ------------

func TestIsSystemModule(t *testing.T) {
	for _, m := range []string{"context_builder", "llm_provider", "index"} {
		if !policy.IsSystemModule(m) {
			t.Errorf("%q should be a system module", m)
		}
	}
	for _, m := range []string{"filesystem", "shell", "memory", ""} {
		if policy.IsSystemModule(m) {
			t.Errorf("%q should NOT be a system module", m)
		}
	}
}

func TestIsMetaTool(t *testing.T) {
	for _, a := range []string{"search_tools", "get_tool", "execute_tool",
		"list_categories", "browse_category", "run_parallel",
		"background_run", "use_skill", "call_app", "ask_user"} {
		if !policy.IsMetaTool(a) {
			t.Errorf("%q should be a meta-tool", a)
		}
	}
	for _, a := range []string{"read", "write", "bash", ""} {
		if policy.IsMetaTool(a) {
			t.Errorf("%q should NOT be a meta-tool", a)
		}
	}
}
