package policy_test

import (
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
)

// gate4Case is one row in the precedence truth table. Holding the
// inputs as a struct makes the test exhaustive : every meaningful
// combination of deny/approve/grant entries × caller × default_policy
// is listed once.
type gate4Case struct {
	name       string
	caller     policy.CallerKind
	caps       *schema.CapabilitiesConfig
	module     string
	action     string
	wantKind   policy.DecisionKind
	wantReason string // substring match ; empty = don't check
}

func grant(module string, actions ...string) schema.CapabilityGrant {
	return schema.CapabilityGrant{Module: module, Tools: actions}
}
func grantWithReason(module, reason string, actions ...string) schema.CapabilityGrant {
	return schema.CapabilityGrant{Module: module, Tools: actions, Reason: reason}
}

func run(t *testing.T, c gate4Case) {
	t.Helper()
	inv := policy.Invocation{
		Caller: c.caller, AppID: "app", AgentID: "a", SessionID: "s",
		UserID: "u", Module: c.module, Action: c.action,
	}
	pc := policy.PolicyContext{AppActive: true, Capabilities: c.caps}
	d := policy.Gate4Policy(inv, pc)
	if d.Kind != c.wantKind {
		t.Fatalf("kind = %v, want %v (reason=%q)", d.Kind, c.wantKind, d.Reason)
	}
	if d.Gate != policy.GatePolicy {
		t.Errorf("gate code = %q, want %q", d.Gate, policy.GatePolicy)
	}
	if c.wantReason != "" && !contains(d.Reason, c.wantReason) {
		t.Errorf("reason %q does not contain %q", d.Reason, c.wantReason)
	}
}

func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestGate4_NilCaps_Allow : dev/test mode. Documented in security.md
// "Optional — absence means dev/test mode (no enforcement)".
func TestGate4_NilCaps_Allow(t *testing.T) {
	run(t, gate4Case{
		name:   "nil_caps",
		caller: policy.CallerLLM,
		caps:   nil,
		module: "x", action: "y",
		wantKind: policy.DecisionAllow,
	})
}

// TestGate4_Precedence : exhaustive precedence table — for each
// combination of (action in deny ? approve ? grant ?) × caller ×
// default_policy, assert the documented winner.
//
// The doc precedence (security.md "Resolving a policy") :
//
//	deny > approve > grant > app default_policy
//
// (per-grant default_action_policy intentionally skipped — see
// gate4_policy.go top comment).
func TestGate4_Precedence(t *testing.T) {
	cases := []gate4Case{
		// ---- Only deny matches → Deny (universal) ----
		{
			name:   "deny_only_LLM",
			caller: policy.CallerLLM,
			caps: &schema.CapabilitiesConfig{
				DefaultPolicy: schema.CapAuto,
				Deny:          []schema.CapabilityGrant{grant("filesystem", "delete")},
			},
			module: "filesystem", action: "delete",
			wantKind:   policy.DecisionDeny,
			wantReason: "denied",
		},
		{
			// Doc explicit : deny is universal. Hooks/setup/channels
			// also blocked.
			name:   "deny_only_Hook",
			caller: policy.CallerHook,
			caps: &schema.CapabilitiesConfig{
				DefaultPolicy: schema.CapAuto,
				Deny:          []schema.CapabilityGrant{grant("filesystem", "delete")},
			},
			module: "filesystem", action: "delete",
			wantKind: policy.DecisionDeny,
		},
		{
			name:   "deny_only_Setup",
			caller: policy.CallerSetup,
			caps: &schema.CapabilitiesConfig{
				Deny: []schema.CapabilityGrant{grant("filesystem", "delete")},
			},
			module: "filesystem", action: "delete",
			wantKind: policy.DecisionDeny,
		},

		// ---- Only approve matches → NeedsApproval (LLM) / Allow (non-LLM) ----
		{
			name:   "approve_only_LLM",
			caller: policy.CallerLLM,
			caps: &schema.CapabilitiesConfig{
				DefaultPolicy: schema.CapAuto,
				Approve:       []schema.CapabilityGrant{grant("shell", "bash")},
			},
			module: "shell", action: "bash",
			wantKind: policy.DecisionNeedsApproval,
		},
		{
			// Doc : "approve" only pauses the agent loop. A hook
			// calling the same action falls through.
			name:   "approve_only_Hook_fallsThrough",
			caller: policy.CallerHook,
			caps: &schema.CapabilitiesConfig{
				DefaultPolicy: schema.CapAuto, // catches the fall-through
				Approve:       []schema.CapabilityGrant{grant("shell", "bash")},
			},
			module: "shell", action: "bash",
			wantKind: policy.DecisionAllow,
		},

		// ---- Only grant matches → Allow ----
		{
			name:   "grant_only_LLM",
			caller: policy.CallerLLM,
			caps: &schema.CapabilitiesConfig{
				Grant: []schema.CapabilityGrant{grant("filesystem", "read")},
			},
			module: "filesystem", action: "read",
			wantKind: policy.DecisionAllow,
		},
		{
			name:   "grant_only_Hook",
			caller: policy.CallerHook,
			caps: &schema.CapabilitiesConfig{
				Grant: []schema.CapabilityGrant{grant("filesystem", "read")},
			},
			module: "filesystem", action: "read",
			wantKind: policy.DecisionAllow,
		},

		// ---- Conflict deny + approve : deny wins ----
		{
			name:   "deny_beats_approve",
			caller: policy.CallerLLM,
			caps: &schema.CapabilitiesConfig{
				Deny:    []schema.CapabilityGrant{grant("shell", "bash")},
				Approve: []schema.CapabilityGrant{grant("shell", "bash")},
			},
			module: "shell", action: "bash",
			wantKind: policy.DecisionDeny,
		},

		// ---- Conflict deny + grant : deny wins ----
		{
			name:   "deny_beats_grant",
			caller: policy.CallerLLM,
			caps: &schema.CapabilitiesConfig{
				Deny:  []schema.CapabilityGrant{grant("filesystem", "delete")},
				Grant: []schema.CapabilityGrant{grant("filesystem", "delete")},
			},
			module: "filesystem", action: "delete",
			wantKind: policy.DecisionDeny,
		},

		// ---- Conflict approve + grant : approve wins (LLM) ----
		{
			name:   "approve_beats_grant_LLM",
			caller: policy.CallerLLM,
			caps: &schema.CapabilitiesConfig{
				Approve: []schema.CapabilityGrant{grant("filesystem", "write")},
				Grant:   []schema.CapabilityGrant{grant("filesystem", "write")},
			},
			module: "filesystem", action: "write",
			wantKind: policy.DecisionNeedsApproval,
		},
		{
			// Non-LLM caller bypasses approve, so grant wins for them.
			name:   "approve_bypassed_grantWins_Hook",
			caller: policy.CallerHook,
			caps: &schema.CapabilitiesConfig{
				Approve: []schema.CapabilityGrant{grant("filesystem", "write")},
				Grant:   []schema.CapabilityGrant{grant("filesystem", "write")},
			},
			module: "filesystem", action: "write",
			wantKind: policy.DecisionAllow,
		},

		// ---- All three : deny still wins ----
		{
			name:   "deny_beats_all",
			caller: policy.CallerLLM,
			caps: &schema.CapabilitiesConfig{
				Deny:    []schema.CapabilityGrant{grant("shell", "bash")},
				Approve: []schema.CapabilityGrant{grant("shell", "bash")},
				Grant:   []schema.CapabilityGrant{grant("shell", "bash")},
			},
			module: "shell", action: "bash",
			wantKind: policy.DecisionDeny,
		},

		// ---- No match → default_policy fall-through ----
		{
			name:   "no_match_default_auto",
			caller: policy.CallerLLM,
			caps: &schema.CapabilitiesConfig{
				DefaultPolicy: schema.CapAuto,
			},
			module: "x", action: "y",
			wantKind: policy.DecisionAllow,
		},
		{
			name:   "no_match_default_approve_LLM",
			caller: policy.CallerLLM,
			caps: &schema.CapabilitiesConfig{
				DefaultPolicy: schema.CapApprove,
			},
			module: "x", action: "y",
			wantKind: policy.DecisionNeedsApproval,
		},
		{
			// Non-LLM caller : default approve degrades to allow.
			name:   "no_match_default_approve_Hook",
			caller: policy.CallerHook,
			caps: &schema.CapabilitiesConfig{
				DefaultPolicy: schema.CapApprove,
			},
			module: "x", action: "y",
			wantKind: policy.DecisionAllow,
		},
		{
			name:   "no_match_default_block",
			caller: policy.CallerLLM,
			caps: &schema.CapabilitiesConfig{
				DefaultPolicy: schema.CapBlock,
			},
			module: "x", action: "y",
			wantKind: policy.DecisionDeny,
		},
		{
			// Doc default policy = "approve" when the field is empty.
			name:   "no_match_default_unset_LLM",
			caller: policy.CallerLLM,
			caps:   &schema.CapabilitiesConfig{}, // DefaultPolicy zero
			module: "x", action: "y",
			wantKind: policy.DecisionNeedsApproval,
		},
		{
			name:   "no_match_default_unset_Hook",
			caller: policy.CallerHook,
			caps:   &schema.CapabilitiesConfig{},
			module: "x", action: "y",
			wantKind: policy.DecisionAllow,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { run(t, c) })
	}
}

// TestGate4_EmptyActionsMatchesAllModuleActions : the doc convention
// — a CapabilityGrant with empty actions list covers all actions of
// the module. Used everywhere (deny, approve, grant, hidden_actions).
func TestGate4_EmptyActionsMatchesAllModuleActions(t *testing.T) {
	cases := []gate4Case{
		{
			name:   "deny_empty_actions",
			caller: policy.CallerLLM,
			caps: &schema.CapabilitiesConfig{
				Deny:          []schema.CapabilityGrant{{Module: "workspace"}},
				DefaultPolicy: schema.CapAuto,
			},
			module: "workspace", action: "anything",
			wantKind: policy.DecisionDeny,
		},
		{
			name:   "approve_empty_actions_LLM",
			caller: policy.CallerLLM,
			caps: &schema.CapabilitiesConfig{
				Approve:       []schema.CapabilityGrant{{Module: "shell"}},
				DefaultPolicy: schema.CapAuto,
			},
			module: "shell", action: "anything",
			wantKind: policy.DecisionNeedsApproval,
		},
		{
			name:   "grant_empty_actions",
			caller: policy.CallerLLM,
			caps: &schema.CapabilitiesConfig{
				Grant:         []schema.CapabilityGrant{{Module: "memory"}},
				DefaultPolicy: schema.CapBlock,
			},
			module: "memory", action: "anything",
			wantKind: policy.DecisionAllow,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { run(t, c) })
	}
}

// TestGate4_ReasonSurfacing : the CapabilityGrant.Reason field is
// documented as "human-readable, surfaced on deny events" — verify
// the gate threads it into the Decision.Reason for the audit log
// + the user-facing pending dialog.
func TestGate4_ReasonSurfacing(t *testing.T) {
	customReason := "Shell commands need explicit approval before running."
	caps := &schema.CapabilitiesConfig{
		Approve: []schema.CapabilityGrant{
			grantWithReason("shell", customReason, "bash"),
		},
	}
	d := policy.Gate4Policy(
		policy.Invocation{Caller: policy.CallerLLM, Module: "shell", Action: "bash"},
		policy.PolicyContext{AppActive: true, Capabilities: caps},
	)
	if d.Kind != policy.DecisionNeedsApproval {
		t.Fatalf("kind = %v, want NeedsApproval", d.Kind)
	}
	if d.Reason != customReason {
		t.Fatalf("reason = %q, want %q (Grant.Reason should pass through)",
			d.Reason, customReason)
	}
}

// TestGate4_ModuleMismatch_NoMatch : a grant on module A must not
// match an invocation on module B. The simplest sanity check.
func TestGate4_ModuleMismatch_NoMatch(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		Grant:         []schema.CapabilityGrant{grant("filesystem", "read")},
		DefaultPolicy: schema.CapBlock,
	}
	d := policy.Gate4Policy(
		policy.Invocation{Caller: policy.CallerLLM, Module: "shell", Action: "read"},
		policy.PolicyContext{AppActive: true, Capabilities: caps},
	)
	if d.Kind != policy.DecisionDeny {
		t.Fatalf("got %v, want Deny (no grant matches, default=block)", d.Kind)
	}
}

// TestGate4_ActionMismatch_NoMatch : a grant on (filesystem, read)
// must not match (filesystem, write).
func TestGate4_ActionMismatch_NoMatch(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		Grant:         []schema.CapabilityGrant{grant("filesystem", "read")},
		DefaultPolicy: schema.CapBlock,
	}
	d := policy.Gate4Policy(
		policy.Invocation{Caller: policy.CallerLLM, Module: "filesystem", Action: "write"},
		policy.PolicyContext{AppActive: true, Capabilities: caps},
	)
	if d.Kind != policy.DecisionDeny {
		t.Fatalf("got %v, want Deny (no grant matches, default=block)", d.Kind)
	}
}

// TestGate4_UnknownDefaultPolicy_FailsClosed : forward-compat — if a
// future YAML adds a new policy value the compiler hasn't blocked,
// the runtime fails closed. The reason includes the unknown value
// so debugging is straightforward.
func TestGate4_UnknownDefaultPolicy_FailsClosed(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapabilityPolicy("future_value"),
	}
	d := policy.Gate4Policy(
		policy.Invocation{Caller: policy.CallerLLM, Module: "x", Action: "y"},
		policy.PolicyContext{AppActive: true, Capabilities: caps},
	)
	if d.Kind != policy.DecisionDeny {
		t.Fatalf("got %v, want Deny (unknown policy = fail-closed)", d.Kind)
	}
	if !contains(d.Reason, "future_value") {
		t.Errorf("reason should mention the unknown value : %q", d.Reason)
	}
}

// TestGate4_MultipleEntries_FirstMatchWins : when multiple entries
// could match in the same list (e.g. two deny rows on the same
// module), the gate uses the first matching one. Order in YAML is
// preserved by the YAML decoder so this is deterministic.
func TestGate4_MultipleEntries_FirstMatchWins(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		Deny: []schema.CapabilityGrant{
			grantWithReason("filesystem", "first deny reason", "delete"),
			grantWithReason("filesystem", "second deny reason", "delete"),
		},
	}
	d := policy.Gate4Policy(
		policy.Invocation{Caller: policy.CallerLLM, Module: "filesystem", Action: "delete"},
		policy.PolicyContext{AppActive: true, Capabilities: caps},
	)
	if d.Kind != policy.DecisionDeny {
		t.Fatalf("got %v, want Deny", d.Kind)
	}
	if d.Reason != "first deny reason" {
		t.Errorf("reason = %q, want 'first deny reason' (order should be stable)", d.Reason)
	}
}
