package policy_test

import (
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/runtime/policy"
)

func mk(caller policy.CallerKind, module, action string) policy.Invocation {
	return policy.Invocation{
		Caller:    caller,
		AppID:     "test-app",
		AgentID:   "main",
		SessionID: "sess-1",
		UserID:    "user-1",
		Module:    module,
		Action:    action,
	}
}

func ctx(appActive bool, caps *schema.CapabilitiesConfig, spec *tool.Spec) policy.PolicyContext {
	return policy.PolicyContext{
		AppActive:    appActive,
		Capabilities: caps,
		AgentModules: nil,
		ToolSpec:     spec,
	}
}

func TestGate0_AppActive_Allow(t *testing.T) {
	for _, c := range []policy.CallerKind{
		policy.CallerLLM, policy.CallerHook, policy.CallerSetup,
		policy.CallerChannel, policy.CallerInternal,
	} {
		d := policy.Gate0Inactive(mk(c, "filesystem", "read"), ctx(true, nil, nil))
		if d.Kind != policy.DecisionAllow {
			t.Errorf("caller=%v active=true : got %v, want Allow", c, d.Kind)
		}
		if d.Gate != policy.GateInactive {
			t.Errorf("caller=%v : gate code = %q, want %q", c, d.Gate, policy.GateInactive)
		}
	}
}

func TestGate0_AppInactive_DenyForAllCallers(t *testing.T) {
	for _, c := range []policy.CallerKind{
		policy.CallerLLM, policy.CallerHook, policy.CallerSetup,
		policy.CallerChannel, policy.CallerInternal,
	} {
		d := policy.Gate0Inactive(mk(c, "filesystem", "read"), ctx(false, nil, nil))
		if d.Kind != policy.DecisionDeny {
			t.Errorf("caller=%v active=false : got %v, want Deny", c, d.Kind)
		}
		if d.Gate != policy.GateInactive {
			t.Errorf("gate code = %q, want %q", d.Gate, policy.GateInactive)
		}
		if d.Reason == "" {
			t.Errorf("caller=%v : empty Reason on Deny", c)
		}
	}
}

func TestGate1a_NonLLMCaller_Bypass(t *testing.T) {
	pc := ctx(true, nil, nil)
	pc.AgentModules = map[string]agentModuleAccessHelper{}
	for _, c := range []policy.CallerKind{
		policy.CallerHook, policy.CallerSetup, policy.CallerChannel, policy.CallerInternal,
	} {
		d := policy.Gate1aModule(mk(c, "filesystem", "read"), pc)
		if d.Kind != policy.DecisionAllow {
			t.Errorf("caller=%v : got %v, want Allow (non-LLM should bypass)", c, d.Kind)
		}
	}
}

func TestGate1a_LLMCaller_NoRestriction_Allow(t *testing.T) {
	d := policy.Gate1aModule(mk(policy.CallerLLM, "filesystem", "read"), ctx(true, nil, nil))
	if d.Kind != policy.DecisionAllow {
		t.Fatalf("got %v, want Allow", d.Kind)
	}
}

func TestGate1a_LLMCaller_ModuleNotInSubset_Deny(t *testing.T) {
	pc := ctx(true, nil, nil)
	pc.AgentModules = map[string]agentModuleAccessHelper{
		"memory": {AllActions: true},
	}
	d := policy.Gate1aModule(mk(policy.CallerLLM, "filesystem", "read"), pc)
	if d.Kind != policy.DecisionDeny {
		t.Fatalf("got %v, want Deny (filesystem not in agent's modules)", d.Kind)
	}
	if d.Gate != policy.GateModule {
		t.Errorf("gate code = %q, want %q", d.Gate, policy.GateModule)
	}
}

func TestGate1a_LLMCaller_ActionNotInSubset_Deny(t *testing.T) {
	pc := ctx(true, nil, nil)
	pc.AgentModules = map[string]agentModuleAccessHelper{
		"filesystem": {Actions: map[string]struct{}{"read": {}, "glob": {}}},
	}
	d := policy.Gate1aModule(mk(policy.CallerLLM, "filesystem", "write"), pc)
	if d.Kind != policy.DecisionDeny {
		t.Fatalf("got %v, want Deny (write not in [read, glob])", d.Kind)
	}
}

func TestGate1a_LLMCaller_ActionInSubset_Allow(t *testing.T) {
	pc := ctx(true, nil, nil)
	pc.AgentModules = map[string]agentModuleAccessHelper{
		"filesystem": {Actions: map[string]struct{}{"read": {}}},
	}
	d := policy.Gate1aModule(mk(policy.CallerLLM, "filesystem", "read"), pc)
	if d.Kind != policy.DecisionAllow {
		t.Fatalf("got %v, want Allow", d.Kind)
	}
}

func TestGate1a_LLMCaller_AllActionsModule_Allow(t *testing.T) {
	pc := ctx(true, nil, nil)
	pc.AgentModules = map[string]agentModuleAccessHelper{
		"shell": {AllActions: true},
	}
	d := policy.Gate1aModule(mk(policy.CallerLLM, "shell", "bash"), pc)
	if d.Kind != policy.DecisionAllow {
		t.Fatalf("got %v, want Allow", d.Kind)
	}
}

func TestGate1b_NonLLMCaller_Bypass(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		HiddenActions: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"glob"}},
		},
	}
	for _, c := range []policy.CallerKind{
		policy.CallerHook, policy.CallerSetup, policy.CallerChannel, policy.CallerInternal,
	} {
		d := policy.Gate1bHidden(mk(c, "filesystem", "glob"), ctx(true, caps, nil))
		if d.Kind != policy.DecisionAllow {
			t.Errorf("caller=%v : got %v, want Allow (hidden bypassed for non-LLM)",
				c, d.Kind)
		}
	}
}

func TestGate1b_LLMCaller_ActionHidden_Deny(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		HiddenActions: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"glob"}},
		},
	}
	d := policy.Gate1bHidden(mk(policy.CallerLLM, "filesystem", "glob"), ctx(true, caps, nil))
	if d.Kind != policy.DecisionDeny {
		t.Fatalf("got %v, want Deny (glob is hidden)", d.Kind)
	}
	if d.Gate != policy.GateHidden {
		t.Errorf("gate code = %q, want %q", d.Gate, policy.GateHidden)
	}
}

func TestGate1b_LLMCaller_ModuleHidden_Deny(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		HiddenModules: []string{"shell"},
	}
	d := policy.Gate1bHidden(mk(policy.CallerLLM, "shell", "bash"), ctx(true, caps, nil))
	if d.Kind != policy.DecisionDeny {
		t.Fatalf("got %v, want Deny (whole shell module hidden)", d.Kind)
	}
}

func TestGate1b_LLMCaller_EmptyToolsList_HidesAll(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		HiddenActions: []schema.CapabilityGrant{
			{Module: "filesystem"},
		},
	}
	d := policy.Gate1bHidden(mk(policy.CallerLLM, "filesystem", "read"), ctx(true, caps, nil))
	if d.Kind != policy.DecisionDeny {
		t.Fatalf("got %v, want Deny (empty tools list hides all)", d.Kind)
	}
}

func TestGate1b_LLMCaller_NotHidden_Allow(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		HiddenActions: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"glob"}},
		},
	}
	d := policy.Gate1bHidden(mk(policy.CallerLLM, "filesystem", "read"), ctx(true, caps, nil))
	if d.Kind != policy.DecisionAllow {
		t.Fatalf("got %v, want Allow (read isn't hidden)", d.Kind)
	}
}

func TestGate1b_LLMCaller_NilCapabilities_Allow(t *testing.T) {
	d := policy.Gate1bHidden(mk(policy.CallerLLM, "x", "y"), ctx(true, nil, nil))
	if d.Kind != policy.DecisionAllow {
		t.Fatalf("got %v, want Allow with nil caps", d.Kind)
	}
}

func spec(name string, level tool.RiskLevel) *tool.Spec {
	return &tool.Spec{Name: name, RiskLevel: level}
}

func TestGate2_NonLLMCaller_Bypass(t *testing.T) {
	caps := &schema.CapabilitiesConfig{MaxRiskLevel: schema.RiskLevel(tool.RiskMedium)}
	s := spec("bash", tool.RiskHigh)
	for _, c := range []policy.CallerKind{
		policy.CallerHook, policy.CallerSetup, policy.CallerChannel, policy.CallerInternal,
	} {
		d := policy.Gate2Risk(mk(c, "shell", "bash"), ctx(true, caps, s))
		if d.Kind != policy.DecisionAllow {
			t.Errorf("caller=%v high-risk : got %v, want Allow (non-LLM bypass)",
				c, d.Kind)
		}
	}
}

func TestGate2_LLMCaller_ActionAtCeiling_Allow(t *testing.T) {
	caps := &schema.CapabilitiesConfig{MaxRiskLevel: schema.RiskLevel(tool.RiskMedium)}
	s := spec("write", tool.RiskMedium)
	d := policy.Gate2Risk(mk(policy.CallerLLM, "filesystem", "write"), ctx(true, caps, s))
	if d.Kind != policy.DecisionAllow {
		t.Fatalf("got %v, want Allow (medium == medium)", d.Kind)
	}
}

func TestGate2_LLMCaller_ActionBelowCeiling_Allow(t *testing.T) {
	caps := &schema.CapabilitiesConfig{MaxRiskLevel: schema.RiskLevel(tool.RiskMedium)}
	s := spec("read", tool.RiskLow)
	d := policy.Gate2Risk(mk(policy.CallerLLM, "filesystem", "read"), ctx(true, caps, s))
	if d.Kind != policy.DecisionAllow {
		t.Fatalf("got %v, want Allow (low < medium)", d.Kind)
	}
}

func TestGate2_LLMCaller_ActionAboveCeiling_Deny(t *testing.T) {
	caps := &schema.CapabilitiesConfig{MaxRiskLevel: schema.RiskLevel(tool.RiskMedium)}
	s := spec("bash", tool.RiskHigh)
	d := policy.Gate2Risk(mk(policy.CallerLLM, "shell", "bash"), ctx(true, caps, s))
	if d.Kind != policy.DecisionDeny {
		t.Fatalf("got %v, want Deny (high > medium)", d.Kind)
	}
	if d.Gate != policy.GateRisk {
		t.Errorf("gate code = %q, want %q", d.Gate, policy.GateRisk)
	}
}

func TestGate2_LLMCaller_DefaultCeiling_IsMedium(t *testing.T) {
	caps := &schema.CapabilitiesConfig{}
	s := spec("bash", tool.RiskHigh)
	d := policy.Gate2Risk(mk(policy.CallerLLM, "shell", "bash"), ctx(true, caps, s))
	if d.Kind != policy.DecisionDeny {
		t.Fatalf("got %v, want Deny (default ceiling is medium, blocks high)", d.Kind)
	}
}

// TestGate2_LLMCaller_NilSpec_Deny : no spec = cannot assess risk =
// fail closed.
func TestGate2_LLMCaller_NilSpec_Deny(t *testing.T) {
	caps := &schema.CapabilitiesConfig{MaxRiskLevel: schema.RiskLevel(tool.RiskHigh)}
	d := policy.Gate2Risk(mk(policy.CallerLLM, "unknown", "action"), ctx(true, caps, nil))
	if d.Kind != policy.DecisionDeny {
		t.Fatalf("got %v, want Deny when ToolSpec is nil (fail-closed)", d.Kind)
	}
}

// TestGate2_LLMCaller_UnknownRiskLevel_Deny : the fail-OPEN regression. An
// action whose risk_level is empty or an unrecognised value must NOT slip under
// the ceiling — riskRank returns -1 for it and a naive `actual <= ceiling`
// (-1 <= anything) would silently bypass max_risk_level. It must fail CLOSED.
func TestGate2_LLMCaller_UnknownRiskLevel_Deny(t *testing.T) {
	caps := &schema.CapabilitiesConfig{MaxRiskLevel: schema.RiskLevel(tool.RiskHigh)} // even the highest ceiling
	for _, lvl := range []tool.RiskLevel{"", "kritikal", "LOW", "unknown"} {
		s := spec("sneaky", lvl)
		d := policy.Gate2Risk(mk(policy.CallerLLM, "shell", "sneaky"), ctx(true, caps, s))
		if d.Kind != policy.DecisionDeny {
			t.Errorf("risk_level=%q : got %v, want Deny (unrankable → fail-closed)", lvl, d.Kind)
		}
	}
}

// TestGate2_LLMCaller_UnknownRiskLevel_GrantBypasses : an explicit grant still
// overrides the cap even for an unrankable action — consistent with the
// documented "explicit grant bypasses the cap" rule (gate 4 deny still applies).
func TestGate2_LLMCaller_UnknownRiskLevel_GrantBypasses(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		MaxRiskLevel: schema.RiskLevel(tool.RiskLow),
		Grant:        []schema.CapabilityGrant{{Module: "shell", Tools: []string{"sneaky"}}},
	}
	s := spec("sneaky", tool.RiskLevel("")) // empty/unrankable
	d := policy.Gate2Risk(mk(policy.CallerLLM, "shell", "sneaky"), ctx(true, caps, s))
	if d.Kind != policy.DecisionAllow {
		t.Fatalf("got %v, want Allow (explicit grant bypasses the cap even for unrankable risk)", d.Kind)
	}
}

// TestGate2_LLMCaller_HighCeiling_AllowsHigh : when an app explicitly
// raises max_risk_level to high, high-risk actions pass.
func TestGate2_LLMCaller_HighCeiling_AllowsHigh(t *testing.T) {
	caps := &schema.CapabilitiesConfig{MaxRiskLevel: schema.RiskLevel(tool.RiskHigh)}
	s := spec("bash", tool.RiskHigh)
	d := policy.Gate2Risk(mk(policy.CallerLLM, "shell", "bash"), ctx(true, caps, s))
	if d.Kind != policy.DecisionAllow {
		t.Fatalf("got %v, want Allow (high <= high)", d.Kind)
	}
}

// ---- Gate 3 — permissions ------------------------------------------
//
// The Python daemon's gate 3 has no YAML surface for declaring an
// "agent permission set". Instead, granted permissions are DERIVED
// from capabilities.grant entries as "{module}:{action}" strings
// (security.py + compiler.py audit). Gate 3 short-circuits when the
// action is explicitly granted ; otherwise it checks each
// required_permission against this derived set.

func specWithPerms(name string, perms []string) *tool.Spec {
	return &tool.Spec{Name: name, RiskLevel: tool.RiskLow, Permissions: perms}
}

// TestGate3_NonLLMCaller_Bypass : hooks/setup/channels bypass gate 3,
// same pattern as gates 1a/1b/2.
func TestGate3_NonLLMCaller_Bypass(t *testing.T) {
	s := specWithPerms("write", []string{"fs.write"})
	pc := ctx(true, nil, s)
	// No grants — would fail for LLM.
	for _, c := range []policy.CallerKind{
		policy.CallerHook, policy.CallerSetup, policy.CallerChannel, policy.CallerInternal,
	} {
		d := policy.Gate3Permissions(mk(c, "filesystem", "write"), pc)
		if d.Kind != policy.DecisionAllow {
			t.Errorf("caller=%v : got %v, want Allow (non-LLM bypass)",
				c, d.Kind)
		}
	}
}

// TestGate3_LLMCaller_NoRequiredPerms_Allow : actions with empty
// required_permissions always pass.
func TestGate3_LLMCaller_NoRequiredPerms_Allow(t *testing.T) {
	s := specWithPerms("read", nil)
	d := policy.Gate3Permissions(mk(policy.CallerLLM, "filesystem", "read"), ctx(true, nil, s))
	if d.Kind != policy.DecisionAllow {
		t.Fatalf("got %v, want Allow (no required perms)", d.Kind)
	}
}

// TestGate3_LLMCaller_ActionGrantedShortcut_Allow : the dominant
// path. If the action is explicitly covered by a grant entry, gate 3
// passes regardless of required_permissions (security.py:337-345
// action_granted shortcut).
func TestGate3_LLMCaller_ActionGrantedShortcut_Allow(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		Grant: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"write"}},
		},
	}
	s := specWithPerms("write", []string{"fs.write"}) // required, but action_granted shortcut applies
	d := policy.Gate3Permissions(mk(policy.CallerLLM, "filesystem", "write"), ctx(true, caps, s))
	if d.Kind != policy.DecisionAllow {
		t.Fatalf("got %v, want Allow (action_granted shortcut)", d.Kind)
	}
}

// TestGate3_LLMCaller_RequiredPermsMatchGranted_Allow : when an
// action requires a permission STRING that coincidentally matches an
// entry in the derived granted set ("module:action" form), gate 3
// passes. Rare in practice but documented behaviour.
func TestGate3_LLMCaller_RequiredPermsMatchGranted_Allow(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		Grant: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"read"}},
		},
	}
	// Action declares its required permission as "filesystem:read"
	// (module:action form, same as the derived granted set entries).
	s := specWithPerms("read", []string{"filesystem:read"})
	d := policy.Gate3Permissions(mk(policy.CallerLLM, "filesystem", "read"), ctx(true, caps, s))
	if d.Kind != policy.DecisionAllow {
		t.Fatalf("got %v, want Allow", d.Kind)
	}
}

// TestGate3_LLMCaller_NoGrants_RequiredPerms_Deny : when the action
// declares required_permissions but there are no grants at all, gate 3
// denies. Mirrors the Python behaviour where the symbolic "fs.write"
// style permissions don't match the empty derived set.
func TestGate3_LLMCaller_NoGrants_RequiredPerms_Deny(t *testing.T) {
	s := specWithPerms("write", []string{"fs.write"})
	d := policy.Gate3Permissions(mk(policy.CallerLLM, "filesystem", "write"), ctx(true, nil, s))
	if d.Kind != policy.DecisionDeny {
		t.Fatalf("got %v, want Deny (no grants, required perms can't match)", d.Kind)
	}
	if d.Gate != policy.GatePermissions {
		t.Errorf("gate code = %q, want %q", d.Gate, policy.GatePermissions)
	}
}

// TestGate3_LLMCaller_GrantOnDifferentAction_NoShortcut_Deny : grant
// exists but on a different action of the same module → no
// action_granted shortcut → required_permissions checked → no match
// → deny.
func TestGate3_LLMCaller_GrantOnDifferentAction_NoShortcut_Deny(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		Grant: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"read"}}, // not "write"
		},
	}
	s := specWithPerms("write", []string{"fs.write"})
	d := policy.Gate3Permissions(mk(policy.CallerLLM, "filesystem", "write"), ctx(true, caps, s))
	if d.Kind != policy.DecisionDeny {
		t.Fatalf("got %v, want Deny", d.Kind)
	}
}

// TestGate3_LLMCaller_NilSpec_Deny : fail-closed when we can't read
// the action's required perms.
func TestGate3_LLMCaller_NilSpec_Deny(t *testing.T) {
	d := policy.Gate3Permissions(mk(policy.CallerLLM, "x", "y"), ctx(true, nil, nil))
	if d.Kind != policy.DecisionDeny {
		t.Fatalf("got %v, want Deny (nil spec)", d.Kind)
	}
}

// ---- helper used by gate 1a tests ----------------------------------

// agentModuleAccessHelper is just an alias for the exported
// policy.AgentModuleAccess — lets the tests use the short name
// without re-typing "policy." everywhere.
type agentModuleAccessHelper = policy.AgentModuleAccess
