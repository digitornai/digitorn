package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/llm"
	dgruntime "github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/context/index"
	"github.com/digitornai/digitorn/internal/runtime/context/meta"
	"github.com/digitornai/digitorn/internal/runtime/context/wiring"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// PL-1 — Lock the 3 meta-paths through the REAL engine gate.
//
// Closes the historic finding "execute_tool bypasses capabilities.deny".
// The security model under test :
//
//   - The meta-tool wrapper itself (context_builder.execute_tool /
//     run_parallel / background_run) bypasses every gate — it's
//     infrastructure, never user-facing. Proven by the absence of any
//     EventSecurityDecision row for the context_builder module (a bypass
//     emits no audit row by design, engine.go emitSecurityDecision).
//   - The DOMAIN sub-tool each one resolves IS gated : a capabilities.deny
//     on filesystem.delete blocks the call no matter which meta-path the
//     model used, the inner dispatcher / background manager never sees it,
//     and a deny audit row is written for filesystem.delete.
//
// Wiring mirrors production bootstrap.go : MetaDispatcher.Gate = engine
// (Engine.GateSubTool → enforceGate → DefaultPolicyEvaluator).
// =====================================================================

// pl1Engine builds an engine for the pl1 app : default_policy=auto,
// max_risk=high, but capabilities.deny on filesystem.delete. The meta
// dispatcher is gated by the engine itself, exactly like production.
func pl1Engine(
	t *testing.T, responses []*llm.ChatResponse,
	inner dgruntime.ToolDispatcher, bg meta.BackgroundManager,
) (*dgruntime.Engine, *projectingSessions) {
	t.Helper()
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
		Deny:          []schema.CapabilityGrant{{Module: "filesystem", Tools: []string{"delete"}}},
	}
	app := secApp("pl1-app", caps, nil)
	apps := &stubApps{app: app}
	sess := newProjectingSessions("pl1-sess")
	lc := &stubLLM{responses: responses}

	// Wire a real ToolSpecLookup so gates 2/3 see each action's risk
	// level instead of fail-closing on a nil spec. Without it every
	// sub-tool is denied for the wrong reason and the allow paths can't
	// be proven.
	lookup := &staticLookup{m: map[string]*tool.Spec{}}
	for _, a := range secUniverse() {
		lookup.m[a.Module+"."+a.Action] = a.Spec
	}

	idx := index.NewBuilder().Build(true, caps, &app.Definition.Agents[0], secUniverse())
	e := newEngine(t, apps, sess, lc)
	e.Context = wiring.New(secStaticActions{all: secUniverse()})
	e.PolicyEvaluator = &dgruntime.DefaultPolicyEvaluator{Lookup: lookup}
	e.Dispatcher = &meta.MetaDispatcher{
		IndexLookup: func(_, _ string) *index.ToolIndex { return idx },
		Inner:       inner,
		Background:  bg,
		Gate:        e,
	}
	return e, sess
}

func pl1Run(t *testing.T, e *dgruntime.Engine) {
	t.Helper()
	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "pl1-app", SessionID: "pl1-sess", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// --- assertion helpers (read events under the session mutex) ---------

func pl1SecurityDecision(sess *projectingSessions, module, action string) *sessionstore.SecurityDecisionPayload {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	for i := range sess.events {
		ev := sess.events[i]
		if ev.Type == sessionstore.EventSecurityDecision && ev.Security != nil &&
			ev.Security.Module == module && ev.Security.Action == action {
			s := *ev.Security
			return &s
		}
	}
	return nil
}

func pl1HasSecurityDecisionForModule(sess *projectingSessions, module string) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	for i := range sess.events {
		ev := sess.events[i]
		if ev.Type == sessionstore.EventSecurityDecision && ev.Security != nil &&
			ev.Security.Module == module {
			return true
		}
	}
	return false
}

func pl1ErroredToolResult(sess *projectingSessions) *sessionstore.ToolPayload {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	for i := range sess.events {
		ev := sess.events[i]
		if ev.Type == sessionstore.EventToolResult && ev.Tool != nil && ev.Tool.Status == "errored" {
			tp := *ev.Tool
			return &tp
		}
	}
	return nil
}

func pl1InnerSaw(r *recordingPerCall, name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range r.called {
		if n == name {
			return true
		}
	}
	return false
}

func pl1InnerCount(r *recordingPerCall) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.called)
}

// =====================================================================
// execute_tool
// =====================================================================

func TestPL1_ExecuteTool_DenyEnforcedThroughEngine(t *testing.T) {
	inner := &recordingPerCall{results: map[string]dgruntime.ToolOutcome{}}
	e, sess := pl1Engine(t, []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID: "c1", Name: "context_builder.execute_tool",
			Arguments: map[string]any{
				"name":   "filesystem.delete",
				"params": map[string]any{"path": "/etc/passwd"},
			},
		}}},
		{Content: "I was blocked"},
	}, inner, nil)

	pl1Run(t, e)

	// Sub-tool gated : the denied tool never reached the inner dispatcher.
	if pl1InnerSaw(inner, "filesystem.delete") {
		t.Error("denied filesystem.delete reached the inner dispatcher via execute_tool")
	}
	// The wrapper's tool_result is errored with a denial reason.
	tr := pl1ErroredToolResult(sess)
	if tr == nil {
		t.Fatal("no errored tool_result for the denied execute_tool")
	}
	if !strings.Contains(tr.Error, "denied") {
		t.Errorf("tool_result error should mention denial : %q", tr.Error)
	}
	// A deny audit row exists for the resolved sub-tool.
	if d := pl1SecurityDecision(sess, "filesystem", "delete"); d == nil || d.Decision != "deny" {
		t.Errorf("expected deny audit row for filesystem.delete, got %+v", d)
	}
	// Half A : the meta-tool wrapper itself was NEVER gated (bypass = no
	// audit row for the context_builder module).
	if pl1HasSecurityDecisionForModule(sess, "context_builder") {
		t.Error("meta-tool wrapper should bypass security : found a context_builder audit row")
	}
}

func TestPL1_ExecuteTool_AllowedSubToolFlowsThrough(t *testing.T) {
	inner := &recordingPerCall{results: map[string]dgruntime.ToolOutcome{}}
	e, sess := pl1Engine(t, []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID: "c1", Name: "context_builder.execute_tool",
			Arguments: map[string]any{
				"name":   "filesystem.read",
				"params": map[string]any{"path": "/etc/hosts"},
			},
		}}},
		{Content: "ok"},
	}, inner, nil)

	pl1Run(t, e)

	// Meta-tool bypass + allowed sub-tool : the call reaches the inner.
	if !pl1InnerSaw(inner, "filesystem.read") {
		t.Error("allowed filesystem.read did not reach the inner dispatcher")
	}
	if pl1ErroredToolResult(sess) != nil {
		t.Error("allowed sub-tool produced an errored tool_result")
	}
}

// =====================================================================
// run_parallel
// =====================================================================

func TestPL1_RunParallel_DenyEnforcedPerChild(t *testing.T) {
	inner := &recordingPerCall{results: map[string]dgruntime.ToolOutcome{}}
	e, sess := pl1Engine(t, []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID: "c1", Name: "context_builder.run_parallel",
			Arguments: map[string]any{"actions": []any{
				map[string]any{"name": "filesystem.read", "params": map[string]any{"path": "/a"}},
				map[string]any{"name": "filesystem.delete", "params": map[string]any{"path": "/b"}},
			}},
		}}},
		{Content: "done"},
	}, inner, nil)

	pl1Run(t, e)

	// Allowed child reached the inner ; denied child did not.
	if !pl1InnerSaw(inner, "filesystem.read") {
		t.Error("allowed filesystem.read did not reach the inner via run_parallel")
	}
	if pl1InnerSaw(inner, "filesystem.delete") {
		t.Error("denied filesystem.delete reached the inner via run_parallel")
	}
	if got := pl1InnerCount(inner); got != 1 {
		t.Errorf("inner reached %d times, want 1 (only the allowed child)", got)
	}
	// Deny audit row for the denied child ; no audit row for the wrapper.
	if d := pl1SecurityDecision(sess, "filesystem", "delete"); d == nil || d.Decision != "deny" {
		t.Errorf("expected deny audit row for filesystem.delete, got %+v", d)
	}
	if pl1HasSecurityDecisionForModule(sess, "context_builder") {
		t.Error("run_parallel wrapper should bypass security : found a context_builder audit row")
	}
}

// =====================================================================
// background_run
// =====================================================================

func TestPL1_BackgroundRun_DenyBlocksLaunchThroughEngine(t *testing.T) {
	rec := &bgIsolationRec{launches: map[string][]string{}}
	mgr := &bgIsolationMgr{rec: rec}
	inner := &recordingPerCall{results: map[string]dgruntime.ToolOutcome{}}

	e, sess := pl1Engine(t, []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID: "c1", Name: "context_builder.background_run",
			Arguments: map[string]any{
				"name":   "filesystem.delete",
				"params": map[string]any{"path": "/x"},
			},
		}}},
		{Content: "blocked"},
	}, inner, mgr)

	pl1Run(t, e)

	// The launch is gated BEFORE the manager schedules it (critical : the
	// manager dispatches later with a tenancy-key AppID that can't be
	// gated downstream).
	rec.mu.Lock()
	launched := len(rec.launches)
	rec.mu.Unlock()
	if launched != 0 {
		t.Errorf("denied tool was launched : %+v", rec.launches)
	}
	tr := pl1ErroredToolResult(sess)
	if tr == nil || !strings.Contains(tr.Error, "denied") {
		t.Errorf("expected errored background_run tool_result mentioning denial, got %+v", tr)
	}
	if d := pl1SecurityDecision(sess, "filesystem", "delete"); d == nil || d.Decision != "deny" {
		t.Errorf("expected deny audit row for filesystem.delete, got %+v", d)
	}
}

func TestPL1_BackgroundRun_AllowedLaunchesThroughEngine(t *testing.T) {
	rec := &bgIsolationRec{launches: map[string][]string{}}
	mgr := &bgIsolationMgr{rec: rec}
	inner := &recordingPerCall{results: map[string]dgruntime.ToolOutcome{}}

	e, _ := pl1Engine(t, []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID: "c1", Name: "context_builder.background_run",
			Arguments: map[string]any{
				"name":   "filesystem.read",
				"params": map[string]any{"path": "/x"},
			},
		}}},
		{Content: "launched"},
	}, inner, mgr)

	pl1Run(t, e)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	total := 0
	for _, ids := range rec.launches {
		total += len(ids)
	}
	if total != 1 {
		t.Errorf("allowed tool launches = %d, want 1 : %+v", total, rec.launches)
	}
}
