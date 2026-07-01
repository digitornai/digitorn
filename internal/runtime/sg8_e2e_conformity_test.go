package runtime_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/policy"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// SG-8 — END-TO-END CONFORMITY TESTS.
//
// Each test below reproduces ONE specific transcript or YAML example
// from the doc and asserts the runtime behaves identically. The doc
// is in line at C:\Users\ASUS\Documents\digitorn-bridge\docs-site\docs ;
// changes there are the contract changes we must mirror.
//
// Scenarios covered :
//
//   * security-02-gates.md           — gates-bot : max_risk_level=medium,
//                                       shell.bash (high) → filtered
//   * security-04-hidden-vs-deny.md  — hidden-bot vs deny-bot : same LLM
//                                       view, different bypass behaviour
//   * advanced-01-sub-agent-isolation.md — reader sub-agent : only
//                                       read/glob/grep visible
//   * security-01-approval.md        — covered by SG-5 sg5_e2e_test.go
//
// To exercise the "what does the LLM see" assertion E2E, we use a
// minimal ToolCatalog adapter that filters the universe of actions
// through BuildAgentToolset (SG-3). This is a preview of what
// CB-1/CB-2 will wire up natively — same logic, but here in test
// scope so we can verify conformity TODAY.

// policyAwareCatalog adapts BuildAgentToolset to the runtime's
// ToolCatalog interface. Each turn it walks the universe with the
// agent's restrictions + capabilities and returns the resulting
// []llm.ToolSpec.
//
// CB-1/CB-2 will replace this with a full-blown ToolRegistry +
// injection planner ; for SG-8 it's enough to verify the filter
// produces the documented output.
type policyAwareCatalog struct {
	universe []policy.AvailableAction
	app      *appmgr.RuntimeApp
}

func (c *policyAwareCatalog) ToolsForAgent(agent *schema.Agent) []llm.ToolSpec {
	if c.app == nil || c.app.Definition == nil || c.app.Definition.Tools == nil {
		return nil
	}
	caps := c.app.Definition.Tools.Capabilities
	active := c.app.Meta != nil && c.app.Meta.Enabled
	visible := policy.BuildAgentToolset(active, caps, agent, c.universe)
	out := make([]llm.ToolSpec, len(visible))
	for i, a := range visible {
		out[i] = llm.ToolSpec{
			Name:        a.Module + "." + a.Action,
			Description: a.Module + " " + a.Action,
		}
	}
	return out
}

// catalogTool is a compact helper to build AvailableAction entries.
func catalogTool(module, action string, risk tool.RiskLevel) policy.AvailableAction {
	return policy.AvailableAction{
		Module: module, Action: action,
		Spec: &tool.Spec{
			Name:      module + "." + action,
			RiskLevel: risk,
		},
	}
}

// runSG8Scenario fires a turn with a stub LLM that emits a single
// text reply (no tool_call), captures the request, and returns the
// list of tool names the LLM was sent. The point is to verify the
// SCHEMA-BUILD filter (SG-3) produces the documented output —
// what the LLM never sees can't be misused.
func runSG8Scenario(
	t *testing.T,
	app *appmgr.RuntimeApp,
	universe []policy.AvailableAction,
) ([]string, *stubLLM) {
	t.Helper()
	apps := &stubApps{app: app}
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{Content: "OK (no tools needed for this assertion)"},
	}}
	cat := &policyAwareCatalog{universe: universe, app: app}

	e := newEngine(t, apps, sess, lc)
	e.Tools = cat

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "approval-bot", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lc.got == nil {
		t.Fatal("no ChatRequest captured")
	}
	names := make([]string, len(lc.got.Tools))
	for i, t := range lc.got.Tools {
		names[i] = t.Name
	}
	return names, lc
}

// hasName reports whether `slice` contains `name`.
func hasName(slice []string, name string) bool {
	for _, n := range slice {
		if n == name {
			return true
		}
	}
	return false
}

// =====================================================================
// Scenario : security-02-gates.md — gates-bot
// =====================================================================

// TestSG8_GatesBot_ReproducesDocTranscript : verbatim replay of
// the live transcript in security-02-gates.md. With
// max_risk_level=medium and shell.bash declared risk=high, the LLM
// must NOT see shell.bash. Filesystem actions in the grant remain.
//
// Doc quote (line 172-177) :
//
//	I don't have a `shell.bash` tool available. The tools I have
//	access to are filesystem tools only: Read, Write, Edit, Glob,
//	Grep, and the orchestration tools ...
func TestSG8_GatesBot_ReproducesDocTranscript(t *testing.T) {
	app := &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: "gates-bot", Enabled: true},
		Definition: &schema.AppDefinition{
			Agents: []schema.Agent{{
				ID: "main", Role: "assistant",
				Brain: schema.Brain{Provider: "openai", Model: "gpt-4o-mini"},
			}},
			Tools: &schema.ToolsBlock{
				Capabilities: &schema.CapabilitiesConfig{
					DefaultPolicy: schema.CapAuto,
					MaxRiskLevel:  schema.RiskLevel(tool.RiskMedium),
					Grant: []schema.CapabilityGrant{
						{Module: "filesystem", Tools: []string{"read", "glob", "grep"}},
					},
				},
			},
		},
	}
	universe := []policy.AvailableAction{
		catalogTool("shell", "bash", tool.RiskHigh),
		catalogTool("filesystem", "read", tool.RiskLow),
		catalogTool("filesystem", "write", tool.RiskMedium),
		catalogTool("filesystem", "edit", tool.RiskMedium),
		catalogTool("filesystem", "glob", tool.RiskLow),
		catalogTool("filesystem", "grep", tool.RiskLow),
	}

	names, _ := runSG8Scenario(t, app, universe)

	// shell.bash MUST be invisible (doc claim).
	if hasName(names, "shell.bash") {
		t.Fatalf("shell.bash visible to LLM (doc says it must NOT be)\nfull list: %v", names)
	}
	// Filesystem actions in grant must be present.
	for _, want := range []string{
		"filesystem.read", "filesystem.glob", "filesystem.grep",
	} {
		if !hasName(names, want) {
			t.Errorf("%s missing from LLM tool list (doc says it must be visible)\nfull list: %v", want, names)
		}
	}
}

// =====================================================================
// Scenario : security-04-hidden-vs-deny.md — hidden-bot & deny-bot
// =====================================================================

// TestSG8_HiddenBot_ReproducesDocBehaviour : with filesystem.glob in
// hidden_actions, the LLM tool list must NOT contain it (doc :
// "schema-builder filtered it out before the LLM saw the tool list").
//
// The other filesystem actions stay because default_policy=auto.
func TestSG8_HiddenBot_ReproducesDocBehaviour(t *testing.T) {
	app := &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: "hidden-bot", Enabled: true},
		Definition: &schema.AppDefinition{
			Agents: []schema.Agent{{
				ID: "main", Role: "assistant",
				Brain: schema.Brain{Provider: "openai", Model: "gpt-4o-mini"},
			}},
			Tools: &schema.ToolsBlock{
				Capabilities: &schema.CapabilitiesConfig{
					DefaultPolicy: schema.CapAuto,
					HiddenActions: []schema.CapabilityGrant{
						{Module: "filesystem", Tools: []string{"glob"}},
					},
				},
			},
		},
	}
	universe := []policy.AvailableAction{
		catalogTool("filesystem", "read", tool.RiskLow),
		catalogTool("filesystem", "write", tool.RiskMedium),
		catalogTool("filesystem", "edit", tool.RiskMedium),
		catalogTool("filesystem", "glob", tool.RiskLow),
		catalogTool("filesystem", "grep", tool.RiskLow),
	}

	names, _ := runSG8Scenario(t, app, universe)

	if hasName(names, "filesystem.glob") {
		t.Fatalf("glob visible despite hidden_actions\nlist: %v", names)
	}
	for _, want := range []string{
		"filesystem.read", "filesystem.write", "filesystem.edit", "filesystem.grep",
	} {
		if !hasName(names, want) {
			t.Errorf("%s missing (only glob should be hidden)\nlist: %v", want, names)
		}
	}
}

// TestSG8_DenyBot_ReproducesDocBehaviour : same as HiddenBot but the
// action is in deny instead. Same LLM-visible result (glob absent)
// — the doc emphasises the LLM perspective is identical.
func TestSG8_DenyBot_ReproducesDocBehaviour(t *testing.T) {
	app := &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: "deny-bot", Enabled: true},
		Definition: &schema.AppDefinition{
			Agents: []schema.Agent{{
				ID: "main", Role: "assistant",
				Brain: schema.Brain{Provider: "openai", Model: "gpt-4o-mini"},
			}},
			Tools: &schema.ToolsBlock{
				Capabilities: &schema.CapabilitiesConfig{
					DefaultPolicy: schema.CapAuto,
					Deny: []schema.CapabilityGrant{
						{Module: "filesystem", Tools: []string{"glob"}},
					},
				},
			},
		},
	}
	universe := []policy.AvailableAction{
		catalogTool("filesystem", "read", tool.RiskLow),
		catalogTool("filesystem", "write", tool.RiskMedium),
		catalogTool("filesystem", "edit", tool.RiskMedium),
		catalogTool("filesystem", "glob", tool.RiskLow),
		catalogTool("filesystem", "grep", tool.RiskLow),
	}

	names, _ := runSG8Scenario(t, app, universe)

	if hasName(names, "filesystem.glob") {
		t.Fatalf("glob visible despite deny\nlist: %v", names)
	}
	for _, want := range []string{
		"filesystem.read", "filesystem.write", "filesystem.edit", "filesystem.grep",
	} {
		if !hasName(names, want) {
			t.Errorf("%s missing\nlist: %v", want, names)
		}
	}
}

// TestSG8_DenyBot_RuntimeAlsoBlocks : deny is different from hidden
// at runtime — a denied action called via the dispatcher (e.g. by
// a hook or a buggy MCP call) must be blocked, while hidden allows
// non-LLM callers through. Verify the runtime path with a denied
// action.
func TestSG8_DenyBot_RuntimeAlsoBlocks(t *testing.T) {
	apps, _ := buildAppWithCaps(t, &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		Deny: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"glob"}},
		},
	})
	sess := &stubSessions{state: okState(t), appendSeq: 1}
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{ID: "c1", Name: "filesystem.glob"}}},
		{Content: "cannot"},
	}}
	disp := &captureDispatcher{}
	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp
	e.PolicyEvaluator = &runtime.DefaultPolicyEvaluator{
		Lookup: &staticLookup{m: map[string]*tool.Spec{
			"filesystem.glob": {Name: "glob", RiskLevel: tool.RiskLow},
		}},
	}

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "app-1", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if disp.count != 0 {
		t.Fatalf("deny must block runtime dispatch, dispatcher count = %d", disp.count)
	}
	tr := sess.findAppend(sessionstore.EventToolResult)
	if tr == nil || tr.Tool == nil || tr.Tool.Status != "errored" {
		t.Fatalf("expected errored tool_result, got %+v", tr)
	}
}

// =====================================================================
// Scenario : advanced-01-sub-agent-isolation.md
// =====================================================================

// TestSG8_SubAgentIsolation_ReproducesDocPattern : the "reader"
// agent with modules: [{filesystem: [read, glob, grep]}, memory]
// only sees those — even when the app capabilities allow all of
// filesystem. Doc claim : "a specialist cannot see a module the
// coordinator doesn't have" (the reverse holds : a specialist
// cannot WIDEN the parent's view either).
func TestSG8_SubAgentIsolation_ReproducesDocPattern(t *testing.T) {
	app := &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: "iso", Enabled: true},
		Definition: &schema.AppDefinition{
			Agents: []schema.Agent{{
				ID:    "reader",
				Brain: schema.Brain{Provider: "openai", Model: "gpt-4o-mini"},
				Modules: schema.AgentModules{
					{ID: "filesystem", Tools: []string{"read", "glob", "grep"}},
					{ID: "memory"},
				},
			}},
			Tools: &schema.ToolsBlock{
				Capabilities: &schema.CapabilitiesConfig{
					DefaultPolicy: schema.CapAuto, // app-wide allows everything
				},
			},
		},
	}
	universe := []policy.AvailableAction{
		catalogTool("filesystem", "read", tool.RiskLow),
		catalogTool("filesystem", "write", tool.RiskMedium),
		catalogTool("filesystem", "edit", tool.RiskMedium),
		catalogTool("filesystem", "glob", tool.RiskLow),
		catalogTool("filesystem", "grep", tool.RiskLow),
		catalogTool("memory", "store", tool.RiskLow),
		catalogTool("memory", "recall", tool.RiskLow),
		catalogTool("shell", "bash", tool.RiskLow), // also tests that shell is filtered (not in agent modules)
	}

	names, _ := runSG8Scenario(t, app, universe)

	mustSee := []string{"filesystem.read", "filesystem.glob", "filesystem.grep",
		"memory.store", "memory.recall"}
	mustNotSee := []string{"filesystem.write", "filesystem.edit", "shell.bash"}

	for _, want := range mustSee {
		if !hasName(names, want) {
			t.Errorf("MUST see %s\nlist: %v", want, names)
		}
	}
	for _, blocked := range mustNotSee {
		if hasName(names, blocked) {
			t.Errorf("MUST NOT see %s (sub-agent widening parent)\nlist: %v", blocked, names)
		}
	}
}

// =====================================================================
// Scenario : security-01-approval.md
// =====================================================================

// TestSG8_ApprovalBot_PointerToSG5E2E : covered in
// sg5_e2e_test.go::TestSG5_E2E_ApprovalBotScenario. The full doc
// transcript step-by-step is asserted there with the real
// sessionstore.Bus + Engine + Registry — this test only verifies
// that the runtime APIs the doc references still exist and are
// reachable (regression guard for accidental removal).
func TestSG8_ApprovalBot_PointerToSG5E2E(t *testing.T) {
	// Smoke check : the runtime exposes the symbols the doc relies on.
	if _, ok := any(&runtime.DefaultPolicyEvaluator{}).(interface {
		Evaluate(context.Context, runtime.EvaluateInput) policy.Decision
	}); !ok {
		t.Fatal("DefaultPolicyEvaluator must implement PolicyEvaluator")
	}
	if !policy.IsSystemModule("context_builder") {
		t.Fatal("context_builder system module bypass missing")
	}
	t.Logf("✓ approval-bot E2E covered in sg5_e2e_test.go::TestSG5_E2E_ApprovalBotScenario")
}

// =====================================================================
// Whole-suite conformity wrap
// =====================================================================

// TestSG8_AllScenariosCovered_DocReference : prints the doc reference
// chart so a CI failure clearly shows which doc transcript broke
// and where to look. No assertions — pure forensics aid.
func TestSG8_AllScenariosCovered_DocReference(t *testing.T) {
	t.Log("\nSG-8 doc conformity coverage map:\n" +
		"  security-02-gates.md (gate 2 risk)             → TestSG8_GatesBot_ReproducesDocTranscript\n" +
		"  security-04-hidden-vs-deny.md (hidden)         → TestSG8_HiddenBot_ReproducesDocBehaviour\n" +
		"  security-04-hidden-vs-deny.md (deny LLM view)  → TestSG8_DenyBot_ReproducesDocBehaviour\n" +
		"  security-04-hidden-vs-deny.md (deny runtime)   → TestSG8_DenyBot_RuntimeAlsoBlocks\n" +
		"  advanced-01-sub-agent-isolation.md             → TestSG8_SubAgentIsolation_ReproducesDocPattern\n" +
		"  security-01-approval.md (full flow)            → TestSG5_E2E_ApprovalBotScenario (in sg5_e2e_test.go)\n" +
		"  security-01-approval.md (denying)              → TestSG5_E2E_DenyScenario (in sg5_e2e_test.go)\n" +
		"  security-01-approval.md (deny>approve)         → TestSG5_E2E_ResolutionOrder_DenyOverApprove\n")
}

// avoid unused-import in case future refactors prune helpers.
var _ = strings.Contains
var _ = time.Now
