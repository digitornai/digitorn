package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// SG-6 tests : the documented audit row (EventSecurityDecision) is
// emitted on every non-bypass policy evaluation, with sensitive
// params redacted, the gate code surfaced, and decision threading
// matched to the doc payload.

// countSecurityEvents counts EventSecurityDecision events with the
// given decision string ("allow"/"deny"/"needs_approval").
func countSecurityEvents(sess *stubSessions, decision string) int {
	n := 0
	for _, ev := range sess.appendEvents {
		if ev.Type == sessionstore.EventSecurityDecision &&
			ev.Security != nil &&
			ev.Security.Decision == decision {
			n++
		}
	}
	return n
}

// findSecurityEvent returns the first EventSecurityDecision in
// the stub session, or nil if none.
func findSecurityEvent(sess *stubSessions) *sessionstore.Event {
	for i := range sess.appendEvents {
		if sess.appendEvents[i].Type == sessionstore.EventSecurityDecision {
			return &sess.appendEvents[i]
		}
	}
	return nil
}

// TestSG6_Allow_EmitsAuditRow : when a tool_call passes every gate,
// exactly one EventSecurityDecision with decision="allow" must be
// emitted, with the documented payload shape.
func TestSG6_Allow_EmitsAuditRow(t *testing.T) {
	apps, _ := buildAppWithCaps(t, &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
	})
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID: "c1", Name: "filesystem.read",
			Arguments: map[string]any{"path": "/etc/hosts"},
		}}},
		{Content: "done"},
	}}
	disp := &captureDispatcher{outcome: runtime.ToolOutcome{Status: "completed"}}
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	e.PolicyEvaluator = &runtime.DefaultPolicyEvaluator{
		Lookup: &staticLookup{m: map[string]*tool.Spec{
			"filesystem.read": {Name: "read", RiskLevel: tool.RiskLow},
		}},
	}

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "user-A",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := countSecurityEvents(sess, "allow"); got != 1 {
		t.Fatalf("allow audit rows = %d, want 1", got)
	}
	ev := findSecurityEvent(sess)
	if ev == nil || ev.Security == nil {
		t.Fatal("no security decision event")
	}
	p := ev.Security

	// Payload conformance check.
	checks := map[string]struct{ got, want string }{
		"AppID":     {p.AppID, "app-1"},
		"SessionID": {p.SessionID, "sess-1"},
		"UserID":    {p.UserID, "user-A"},
		"AgentID":   {p.AgentID, "primary"},
		"Module":    {p.Module, "filesystem"},
		"Action":    {p.Action, "read"},
		"RiskLevel": {p.RiskLevel, "low"},
		"Decision":  {p.Decision, "allow"},
		"Caller":    {p.Caller, "llm"},
	}
	for k, c := range checks {
		if c.got != c.want {
			t.Errorf("payload.%s = %q, want %q", k, c.got, c.want)
		}
	}
	// Gate code should be a real gate (gate4_policy in this Allow path).
	if !strings.HasPrefix(p.Gate, "gate") {
		t.Errorf("Gate = %q, want a gateN_xxx code", p.Gate)
	}
	// Params must be present.
	if p.ParamsRedacted["path"] != "/etc/hosts" {
		t.Errorf("path param missing : %+v", p.ParamsRedacted)
	}
}

// TestSG6_Deny_EmitsAuditRowWithReason : a denied tool_call emits
// decision="deny" + the reason that explains the block.
func TestSG6_Deny_EmitsAuditRowWithReason(t *testing.T) {
	apps, _ := buildAppWithCaps(t, &schema.CapabilitiesConfig{
		Deny: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"delete"}, Reason: "no destructive ops"},
		},
	})
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "filesystem.delete"}}},
		{Content: "fail"},
	}}
	disp := &captureDispatcher{}
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	e.PolicyEvaluator = &runtime.DefaultPolicyEvaluator{
		Lookup: &staticLookup{m: map[string]*tool.Spec{
			"filesystem.delete": {Name: "delete", RiskLevel: tool.RiskLow},
		}},
	}

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := countSecurityEvents(sess, "deny"); got != 1 {
		t.Fatalf("deny audit rows = %d, want 1", got)
	}
	ev := findSecurityEvent(sess)
	if ev.Security.Reason != "no destructive ops" {
		t.Errorf("Reason = %q, want 'no destructive ops'", ev.Security.Reason)
	}
	if ev.Security.Gate != string(policy.GatePolicy) {
		t.Errorf("Gate = %q, want %q", ev.Security.Gate, policy.GatePolicy)
	}
	if disp.count != 0 {
		t.Errorf("dispatcher count = %d, want 0", disp.count)
	}
}

// TestSG6_NeedsApproval_EmitsAuditRow : approve-policy emits
// decision="needs_approval" so consumers see the pending suspension
// in the timeline.
func TestSG6_NeedsApproval_EmitsAuditRow(t *testing.T) {
	apps, _ := buildAppWithCaps(t, &schema.CapabilitiesConfig{
		MaxRiskLevel: schema.RiskLevel(tool.RiskHigh),
		Approve: []schema.CapabilityGrant{
			{Module: "shell", Tools: []string{"bash"}, Reason: "shell approval"},
		},
	})
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "shell.bash"}}},
		{Content: "fallback"},
	}}
	disp := &captureDispatcher{}
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	// No ApprovalRegistry → falls back to errored, but audit row
	// must still fire.
	e.PolicyEvaluator = &runtime.DefaultPolicyEvaluator{
		Lookup: &staticLookup{m: map[string]*tool.Spec{
			"shell.bash": {Name: "bash", RiskLevel: tool.RiskHigh},
		}},
	}

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := countSecurityEvents(sess, "needs_approval"); got != 1 {
		t.Fatalf("needs_approval audit rows = %d, want 1", got)
	}
}

// TestSG6_ParamsRedacted_InAuditRow : sensitive keys (password,
// api_key, token, ...) are replaced by [REDACTED] in the audit row,
// the original args remain intact for the dispatcher.
func TestSG6_ParamsRedacted_InAuditRow(t *testing.T) {
	apps, _ := buildAppWithCaps(t, &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto})
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID: "c1", Name: "http.post",
			Arguments: map[string]any{
				"url":      "https://api.example.com/login",
				"password": "p@ssw0rd",
				"api_key":  "sk-abc-123",
				"headers": map[string]any{
					"Authorization": "Bearer xyz",
				},
			},
		}}},
		{Content: "done"},
	}}
	disp := &captureDispatcher{outcome: runtime.ToolOutcome{Status: "completed"}}
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	e.PolicyEvaluator = &runtime.DefaultPolicyEvaluator{
		Lookup: &staticLookup{m: map[string]*tool.Spec{
			"http.post": {Name: "post", RiskLevel: tool.RiskLow},
		}},
	}

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	ev := findSecurityEvent(sess)
	if ev == nil || ev.Security == nil {
		t.Fatal("no audit row")
	}
	p := ev.Security.ParamsRedacted
	if p["password"] != policy.RedactedPlaceholder {
		t.Errorf("password not redacted : %v", p["password"])
	}
	if p["api_key"] != policy.RedactedPlaceholder {
		t.Errorf("api_key not redacted : %v", p["api_key"])
	}
	if p["url"] != "https://api.example.com/login" {
		t.Errorf("safe url got mangled : %v", p["url"])
	}
	headers, ok := p["headers"].(map[string]any)
	if !ok {
		t.Fatalf("nested headers : %T", p["headers"])
	}
	if headers["Authorization"] != policy.RedactedPlaceholder {
		t.Errorf("nested Authorization not redacted : %v", headers["Authorization"])
	}

	// Importantly : the DISPATCHER received the REAL params (not redacted) —
	// audit row is observability, not control.
	if disp.count != 1 {
		t.Fatalf("dispatcher count = %d, want 1", disp.count)
	}
}

// TestSG6_SystemModule_NoAuditRow : system modules
// (context_builder, llm_provider, index) bypass the gates and
// MUST NOT produce an audit row. Doc Python doesn't log infra-tool
// calls and we mirror that to keep the durable log clean.
func TestSG6_SystemModule_NoAuditRow(t *testing.T) {
	apps, _ := buildAppWithCaps(t, &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapBlock, // would block everything else
		Deny:          []schema.CapabilityGrant{{Module: "context_builder"}},
	})
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "context_builder.list"}}},
		{Content: "done"},
	}}
	disp := &captureDispatcher{outcome: runtime.ToolOutcome{Status: "completed"}}
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	e.PolicyEvaluator = &runtime.DefaultPolicyEvaluator{Lookup: &staticLookup{}}

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := countSecurityEvents(sess, "allow"); got != 0 {
		t.Fatalf("system_module_bypass should produce 0 audit rows, got %d", got)
	}
}

// TestSG6_MetaTool_NoAuditRow : meta-tools (execute_tool,
// search_tools, ...) also bypass and don't produce an audit row.
func TestSG6_MetaTool_NoAuditRow(t *testing.T) {
	apps, _ := buildAppWithCaps(t, &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapBlock,
	})
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "execute_tool"}}},
		{Content: "done"},
	}}
	disp := &captureDispatcher{outcome: runtime.ToolOutcome{Status: "completed"}}
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	e.PolicyEvaluator = &runtime.DefaultPolicyEvaluator{Lookup: &staticLookup{}}

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := len(sess.appendEvents); got > 0 {
		for _, ev := range sess.appendEvents {
			if ev.Type == sessionstore.EventSecurityDecision {
				t.Fatalf("meta-tool bypass should not produce audit row : %+v",
					ev.Security)
			}
		}
	}
}

// TestSG6_NilPolicyEvaluator_NoAuditRows : when no PolicyEvaluator
// is wired (test / dev mode), no audit row is emitted because no
// decision is made.
func TestSG6_NilPolicyEvaluator_NoAuditRows(t *testing.T) {
	apps, _ := buildAppWithCaps(t, nil)
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "filesystem.read"}}},
		{Content: "done"},
	}}
	disp := &captureDispatcher{outcome: runtime.ToolOutcome{Status: "completed"}}
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	// e.PolicyEvaluator intentionally nil

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, ev := range sess.appendEvents {
		if ev.Type == sessionstore.EventSecurityDecision {
			t.Fatalf("nil PolicyEvaluator must not emit audit rows")
		}
	}
}
