package runtime_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
)

// SG-4 tests : the wirage of policy gates into the engine. Each test
// uses a real engine with a PolicyEvaluator configured, asserts that
// the gate decision is propagated to the ToolOutcome correctly, and
// (for the deny / approval branches) verifies the dispatcher was
// NEVER called.

// captureDispatcher counts how many Dispatches actually happened.
// Used to prove that a denied gate short-circuits before the
// dispatcher runs.
type captureDispatcher struct {
	mu      sync.Mutex // guards count : a round with N tool calls dispatches in parallel
	count   int
	outcome runtime.ToolOutcome
}

func (d *captureDispatcher) Dispatch(_ context.Context, _ runtime.ToolInvocation) runtime.ToolOutcome {
	d.mu.Lock()
	d.count++
	d.mu.Unlock()
	if d.outcome.Status == "" {
		return runtime.ToolOutcome{Status: "completed"}
	}
	return d.outcome
}

// staticLookup is a 1-entry test ToolSpecLookup.
type staticLookup struct {
	m map[string]*tool.Spec
}

func (s *staticLookup) LookupToolSpec(module, action string) *tool.Spec {
	if s == nil {
		return nil
	}
	return s.m[module+"."+action]
}

// buildAppWithCaps : builds an app with the given capabilities config
// and an agent named "main". Defaults : enabled=true, agent has no
// module restriction, no permissions.
func buildAppWithCaps(t *testing.T, caps *schema.CapabilitiesConfig) (*stubApps, *schema.Agent) {
	t.Helper()
	app := okAppBYOK(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"}, false)
	if app.Definition.Tools == nil {
		app.Definition.Tools = &schema.ToolsBlock{}
	}
	if caps != nil {
		app.Definition.Tools.Capabilities = caps
	}
	// okAppBYOK does NOT set Enabled ; gate 0 wants it true. Patch.
	app.Meta.Enabled = true
	return &stubApps{app: app}, &app.Definition.Agents[0]
}

// TestSG4_PolicyEvaluator_NilSkipsEnforcement : when the engine has
// no PolicyEvaluator, the dispatcher runs as before — proves RT-3
// behaviour is preserved.
func TestSG4_PolicyEvaluator_NilSkipsEnforcement(t *testing.T) {
	apps, _ := buildAppWithCaps(t, &schema.CapabilitiesConfig{
		Deny: []schema.CapabilityGrant{{Module: "filesystem"}}, // would deny
	})
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "filesystem.read"}}},
		{Content: "done"},
	}}
	disp := &captureDispatcher{}

	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	// e.PolicyEvaluator left nil — no enforcement

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if disp.count != 1 {
		t.Fatalf("dispatcher count = %d, want 1 (nil PolicyEvaluator = no enforcement)", disp.count)
	}
}

// TestSG4_PolicyDeny_BlocksDispatch : with a real PolicyEvaluator
// configured, a denied tool_call never reaches the dispatcher and
// the outcome carries an "errored" status with the gate's reason.
func TestSG4_PolicyDeny_BlocksDispatch(t *testing.T) {
	apps, _ := buildAppWithCaps(t, &schema.CapabilitiesConfig{
		Deny: []schema.CapabilityGrant{{Module: "filesystem", Tools: []string{"delete"}}},
	})
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "filesystem.delete"}}},
		{Content: "couldnt"},
	}}
	disp := &captureDispatcher{}
	lookup := &staticLookup{m: map[string]*tool.Spec{
		"filesystem.delete": {Name: "delete", RiskLevel: tool.RiskLow},
	}}

	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	e.PolicyEvaluator = &runtime.DefaultPolicyEvaluator{Lookup: lookup}

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if disp.count != 0 {
		t.Fatalf("dispatcher count = %d, want 0 (deny must short-circuit)", disp.count)
	}
	// Find the tool_result event and check it's errored with the gate code.
	ev := sess.findAppend("tool_result")
	if ev == nil || ev.Tool == nil {
		t.Fatal("no tool_result event")
	}
	if ev.Tool.Status != "errored" {
		t.Fatalf("status = %q, want errored", ev.Tool.Status)
	}
	if !strings.Contains(ev.Tool.Error, string(policy.GatePolicy)) {
		t.Errorf("error should mention gate code %q : %q", policy.GatePolicy, ev.Tool.Error)
	}
}

// TestSG4_PolicyApprove_BlocksDispatchUntilSG5 : an approve-policy
// action does not reach the dispatcher either. The outcome carries
// a clear "approval required" message indicating SG-5 will replace
// this behaviour.
func TestSG4_PolicyApprove_BlocksDispatchUntilSG5(t *testing.T) {
	apps, _ := buildAppWithCaps(t, &schema.CapabilitiesConfig{
		Approve: []schema.CapabilityGrant{
			{Module: "shell", Tools: []string{"bash"},
				Reason: "shell needs explicit approval"},
		},
		MaxRiskLevel: schema.RiskLevel(tool.RiskHigh), // so gate 2 doesn't deny first
	})
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "shell.bash"}}},
		{Content: "fallback"},
	}}
	disp := &captureDispatcher{}
	lookup := &staticLookup{m: map[string]*tool.Spec{
		"shell.bash": {Name: "bash", RiskLevel: tool.RiskHigh},
	}}

	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	e.PolicyEvaluator = &runtime.DefaultPolicyEvaluator{Lookup: lookup}

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if disp.count != 0 {
		t.Fatalf("dispatcher count = %d, want 0 (approve must short-circuit)", disp.count)
	}
	ev := sess.findAppend("tool_result")
	if ev == nil || ev.Tool == nil {
		t.Fatal("no tool_result event")
	}
	if ev.Tool.Status != "errored" {
		t.Fatalf("status = %q, want errored", ev.Tool.Status)
	}
	if !strings.Contains(ev.Tool.Error, "approval") {
		t.Errorf("error should mention approval : %q", ev.Tool.Error)
	}
}

// TestSG4_PolicyAllow_PassesToDispatch : when every gate passes, the
// dispatcher runs. The result is the dispatcher's outcome, unchanged.
func TestSG4_PolicyAllow_PassesToDispatch(t *testing.T) {
	apps, _ := buildAppWithCaps(t, &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
	})
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "filesystem.read"}}},
		{Content: "done"},
	}}
	disp := &captureDispatcher{}
	lookup := &staticLookup{m: map[string]*tool.Spec{
		"filesystem.read": {Name: "read", RiskLevel: tool.RiskLow},
	}}

	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	e.PolicyEvaluator = &runtime.DefaultPolicyEvaluator{Lookup: lookup}

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if disp.count != 1 {
		t.Fatalf("dispatcher count = %d, want 1 (allow must reach dispatcher)", disp.count)
	}
}

// TestSG4_SystemModuleBypassesPolicy : even with a deny configured,
// a call to a system module (context_builder) reaches the dispatcher.
// Doc invariant : system modules are trusted infrastructure.
func TestSG4_SystemModuleBypassesPolicy(t *testing.T) {
	apps, _ := buildAppWithCaps(t, &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapBlock, // would block everything else
		Deny:          []schema.CapabilityGrant{{Module: "context_builder"}},
	})
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "context_builder.list"}}},
		{Content: "done"},
	}}
	disp := &captureDispatcher{}
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	e.PolicyEvaluator = &runtime.DefaultPolicyEvaluator{
		Lookup: &staticLookup{},
	}

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if disp.count != 1 {
		t.Fatalf("dispatcher count = %d, want 1 (system module bypass)", disp.count)
	}
}

// TestSG4_MetaToolBypassesPolicy : meta-tools (execute_tool,
// search_tools, ...) bypass the gates, including a global deny.
func TestSG4_MetaToolBypassesPolicy(t *testing.T) {
	apps, _ := buildAppWithCaps(t, &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapBlock,
	})
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "execute_tool"}}},
		{Content: "done"},
	}}
	disp := &captureDispatcher{}
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	e.PolicyEvaluator = &runtime.DefaultPolicyEvaluator{Lookup: &staticLookup{}}

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if disp.count != 1 {
		t.Fatalf("dispatcher count = %d, want 1 (meta-tool bypass)", disp.count)
	}
}

// TestSG4_InactiveApp_BlocksAllCalls : when app.Meta.Enabled is
// false, gate 0 denies every tool_call.
func TestSG4_InactiveApp_BlocksAllCalls(t *testing.T) {
	apps, _ := buildAppWithCaps(t, &schema.CapabilitiesConfig{DefaultPolicy: schema.CapAuto})
	apps.app.Meta.Enabled = false // turn off
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "filesystem.read"}}},
		{Content: "couldnt"},
	}}
	disp := &captureDispatcher{}
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	e.PolicyEvaluator = &runtime.DefaultPolicyEvaluator{
		Lookup: &staticLookup{m: map[string]*tool.Spec{
			"filesystem.read": {Name: "read", RiskLevel: tool.RiskLow},
		}},
	}

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if disp.count != 0 {
		t.Fatalf("dispatcher count = %d, want 0 (inactive app)", disp.count)
	}
	ev := sess.findAppend("tool_result")
	if ev == nil || ev.Tool == nil || ev.Tool.Status != "errored" {
		t.Fatalf("expected errored tool_result, got %+v", ev)
	}
	if !strings.Contains(ev.Tool.Error, string(policy.GateInactive)) {
		t.Errorf("error should mention %q : %q", policy.GateInactive, ev.Tool.Error)
	}
}

// TestSG4_RiskOverCeiling_BlocksDispatch : the canonical gate 2
// scenario at runtime. A high-risk action with max_risk_level=medium
// is denied before dispatch.
func TestSG4_RiskOverCeiling_BlocksDispatch(t *testing.T) {
	apps, _ := buildAppWithCaps(t, &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskMedium),
	})
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "shell.bash"}}},
		{Content: "couldnt"},
	}}
	disp := &captureDispatcher{}
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
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
	if disp.count != 0 {
		t.Fatalf("dispatcher count = %d, want 0 (gate 2)", disp.count)
	}
	ev := sess.findAppend("tool_result")
	if !strings.Contains(ev.Tool.Error, string(policy.GateRisk)) {
		t.Errorf("error should mention gate %q : %q", policy.GateRisk, ev.Tool.Error)
	}
}
