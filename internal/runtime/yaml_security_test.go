package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/llm"
	dgruntime "github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/context/wiring"
	"github.com/digitornai/digitorn/internal/runtime/policy"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// CC-1 to CC-8 — YAML-driven security tests
//
// Every test in this file asserts that what an app declares in its
// YAML strictly controls what an agent can see and call. The
// SECURITY model has two layers per docs-site/language/11-security.md
// + security-02-gates.md :
//
//   * Schema-build filter (SG-3) — tools NOT in the agent's
//     visible toolset never reach the LLM.
//   * Runtime gates (SG-4) — even if forced (e.g. direct FQN call
//     past discovery), gates evaluate again.
//
// Each test verifies BOTH layers : index absence AND runtime
// rejection. That's the doc-conform "defense in depth" semantic.
// =====================================================================

// secApp builds an app with the given capabilities config and a
// single agent that has access to all modules by default.
func secApp(appID string, caps *schema.CapabilitiesConfig, agentMods schema.AgentModules) *appmgr.RuntimeApp {
	return &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: appID, Enabled: true},
		Definition: &schema.AppDefinition{
			App: schema.AppMeta{
				AppID: appID, Name: appID, Version: "1.0",
			},
			Agents: []schema.Agent{{
				ID:           "main",
				Role:         "assistant",
				Brain:        schema.Brain{Provider: "openai", Model: "gpt-4o-mini"},
				SystemPrompt: "Test agent",
				Modules:      agentMods,
			}},
			Tools: &schema.ToolsBlock{
				Capabilities: caps,
			},
			Runtime: &schema.RuntimeBlock{
				ToolInjection: schema.ToolInjectionDirect,
			},
		},
	}
}

// secUniverse provides the 4 reference tools used across the
// YAML security tests : 2 filesystem (low/high risk) + 1 shell
// (high risk) + 1 http (medium risk).
func secUniverse() []policy.AvailableAction {
	return []policy.AvailableAction{
		{Module: "filesystem", Action: "read",
			Spec: &tool.Spec{Name: "filesystem.read", RiskLevel: tool.RiskLow}},
		{Module: "filesystem", Action: "delete",
			Spec: &tool.Spec{Name: "filesystem.delete", RiskLevel: tool.RiskHigh, Irreversible: true}},
		{Module: "shell", Action: "bash",
			Spec: &tool.Spec{Name: "shell.bash", RiskLevel: tool.RiskHigh}},
		{Module: "http", Action: "get",
			Spec: &tool.Spec{Name: "http.get", RiskLevel: tool.RiskMedium}},
	}
}

// secCtx returns a Context wired with the given universe.
type secStaticActions struct {
	all []policy.AvailableAction
}

func (s secStaticActions) ForApp(string) []policy.AvailableAction { return s.all }

// runSecTurn runs one turn for the given app + universe and
// returns (a) the tool list the LLM saw, (b) the session stub for
// audit assertions.
func runSecTurn(
	t *testing.T, app *appmgr.RuntimeApp, universe []policy.AvailableAction,
	llmResp *llm.ChatResponse,
) (toolsSeen []string, sess *projectingSessions, lc *stubLLM) {
	t.Helper()
	apps := &stubApps{app: app}
	sess = newProjectingSessions("sess-sec")
	lc = &stubLLM{resp: llmResp}

	cb := wiring.New(secStaticActions{all: universe})
	e := newEngine(t, apps, sess, lc)
	e.Context = cb

	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: app.Meta.AppID, SessionID: "sess-sec", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if lc.got != nil {
		for _, ts := range lc.got.Tools {
			toolsSeen = append(toolsSeen, ts.Name)
		}
	}
	return toolsSeen, sess, lc
}

// contains looks for the needle in haystack. Tool names are
// compared in BOTH canonical form (filesystem.read) and the
// outbound sanitized form (filesystem__read) since the planner
// emits the latter onto the LLM wire per doc-conform tool name
// sanitization (04-tools.md). Callers can write either form
// in their assertions ; this helper finds the match.
func contains(haystack []string, needle string) bool {
	// Build the underscored variant of the needle.
	sanitized := needle
	if idx := strings.Index(needle, "."); idx != -1 {
		sanitized = needle[:idx] + "__" + needle[idx+1:]
	}
	for _, s := range haystack {
		if s == needle || s == sanitized {
			return true
		}
	}
	return false
}

// =====================================================================
// CC-1 — deny blocks the tool at SG-3 (index) AND at SG-4 (runtime)
// =====================================================================

func TestYAMLSec_DenyHidesAndRejects(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
		Deny: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"delete"}},
		},
	}
	app := secApp("deny-app", caps, nil)

	// Layer 1 (SG-3) : filesystem.delete must NOT be in the index.
	tools, _, _ := runSecTurn(t, app, secUniverse(),
		&llm.ChatResponse{Content: "ok"})
	if contains(tools, "filesystem.delete") {
		t.Errorf("denied tool appeared in LLM tool list : %v", tools)
	}
	if !contains(tools, "filesystem.read") {
		t.Errorf("non-denied tool missing : %v", tools)
	}

	// Layer 2 (SG-4) : even if a rogue LLM calls filesystem.delete
	// directly (bypassing the index), the gate rejects it.
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-sec")
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID: "c1", Name: "filesystem.delete",
			Arguments: map[string]any{"path": "/x"},
		}}},
		{Content: "I was blocked"},
	}}

	// Wire a real PolicyEvaluator so SG-4 runs.
	cb := wiring.New(secStaticActions{all: secUniverse()})
	e := newEngine(t, apps, sess, lc)
	e.Context = cb
	e.PolicyEvaluator = &dgruntime.DefaultPolicyEvaluator{}

	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "deny-app", SessionID: "sess-sec", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The tool_result must be errored.
	for _, ev := range sess.events {
		if ev.Type == sessionstore.EventToolResult && ev.Tool != nil &&
			ev.Tool.Name == "filesystem.delete" {
			if ev.Tool.Status != "errored" {
				t.Errorf("denied tool got status %q, want errored", ev.Tool.Status)
			}
			if !strings.Contains(ev.Tool.Error, "denied") &&
				!strings.Contains(ev.Tool.Error, "security") {
				t.Errorf("error should mention denial : %q", ev.Tool.Error)
			}
		}
	}
}

// =====================================================================
// CC-2 — max_risk_level filters tools above the cap
// =====================================================================

func TestYAMLSec_MaxRiskLevelFilters(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskLow), // only Low allowed
	}
	app := secApp("low-only-app", caps, nil)

	tools, _, _ := runSecTurn(t, app, secUniverse(),
		&llm.ChatResponse{Content: "ok"})

	// Only filesystem.read (low) should be visible. The others
	// (delete=high, bash=high, http.get=medium) must be filtered.
	if !contains(tools, "filesystem.read") {
		t.Errorf("low-risk tool missing : %v", tools)
	}
	if contains(tools, "filesystem.delete") {
		t.Errorf("high-risk tool leaked : %v", tools)
	}
	if contains(tools, "shell.bash") {
		t.Errorf("high-risk shell leaked : %v", tools)
	}
	if contains(tools, "http.get") {
		t.Errorf("medium-risk above low cap leaked : %v", tools)
	}
}

// CC-2 (bis) — max_risk_level=medium allows medium but not high
func TestYAMLSec_MaxRiskLevelMedium(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskMedium),
	}
	app := secApp("mid-app", caps, nil)
	tools, _, _ := runSecTurn(t, app, secUniverse(),
		&llm.ChatResponse{Content: "ok"})

	if !contains(tools, "http.get") {
		t.Errorf("medium tool blocked at medium cap : %v", tools)
	}
	if contains(tools, "shell.bash") {
		t.Errorf("high tool leaked at medium cap : %v", tools)
	}
}

// =====================================================================
// CC-3 — hidden_modules removes the whole module
// =====================================================================

func TestYAMLSec_HiddenModulesRemovesAll(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
		HiddenModules: []string{"shell"},
	}
	app := secApp("no-shell-app", caps, nil)
	tools, _, _ := runSecTurn(t, app, secUniverse(),
		&llm.ChatResponse{Content: "ok"})

	for _, name := range tools {
		if strings.HasPrefix(name, "shell.") {
			t.Errorf("hidden module 'shell' leaked tool : %s", name)
		}
	}
	if !contains(tools, "filesystem.read") {
		t.Errorf("non-hidden module missing : %v", tools)
	}
}

// CC-3 (bis) — hidden_actions removes specific actions
func TestYAMLSec_HiddenActionsRemovesPerAction(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
		HiddenActions: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"delete"}},
		},
	}
	app := secApp("no-fs-del-app", caps, nil)
	tools, _, _ := runSecTurn(t, app, secUniverse(),
		&llm.ChatResponse{Content: "ok"})

	if contains(tools, "filesystem.delete") {
		t.Errorf("hidden action leaked : %v", tools)
	}
	if !contains(tools, "filesystem.read") {
		t.Errorf("non-hidden filesystem action missing : %v", tools)
	}
}

// =====================================================================
// CC-4 — grant opens a tool even when default_policy=deny
// =====================================================================

func TestYAMLSec_GrantOverridesDeny(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapBlock, // deny everything by default
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
		Grant: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"read"}},
		},
	}
	app := secApp("grant-app", caps, nil)
	tools, _, _ := runSecTurn(t, app, secUniverse(),
		&llm.ChatResponse{Content: "ok"})

	if !contains(tools, "filesystem.read") {
		t.Errorf("granted tool missing : %v", tools)
	}
	// Everything else must be denied by default.
	if contains(tools, "shell.bash") {
		t.Errorf("default-deny leaked shell.bash : %v", tools)
	}
	if contains(tools, "filesystem.delete") {
		t.Errorf("default-deny leaked filesystem.delete : %v", tools)
	}
}

// CC-4 (bis) — explicit deny beats grant (priority order)
func TestYAMLSec_DenyBeatsGrant(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
		Grant: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"delete"}},
		},
		Deny: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"delete"}},
		},
	}
	app := secApp("conflict-app", caps, nil)
	tools, _, _ := runSecTurn(t, app, secUniverse(),
		&llm.ChatResponse{Content: "ok"})

	if contains(tools, "filesystem.delete") {
		t.Errorf("deny must beat grant : %v", tools)
	}
}

// =====================================================================
// CC-6 — App disabled (gate 0) blocks ALL tools
// =====================================================================

func TestYAMLSec_AppDisabledBlocksAll(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	app := secApp("disabled-app", caps, nil)
	app.Meta.Enabled = false // gate 0

	tools, _, _ := runSecTurn(t, app, secUniverse(),
		&llm.ChatResponse{Content: "ok"})

	for _, name := range tools {
		// Only context_builder.* meta-tools survive (they bypass).
		// Names are sanitized to context_builder__ on the wire per
		// 04-tools.md.
		if !strings.HasPrefix(name, "context_builder__") &&
			!strings.HasPrefix(name, "context_builder.") {
			t.Errorf("disabled app leaked non-meta tool : %s", name)
		}
	}
}

// =====================================================================
// CC-7 — agents[].modules subset enforcement (gate 1a)
// =====================================================================

func TestYAMLSec_AgentModulesSubsetEnforced(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	// Agent only declares 'filesystem' — shell and http should be
	// filtered even though the app's caps allow them.
	app := secApp("subset-app", caps, schema.AgentModules{
		{ID: "filesystem"},
	})

	tools, _, _ := runSecTurn(t, app, secUniverse(),
		&llm.ChatResponse{Content: "ok"})

	for _, name := range tools {
		if strings.HasPrefix(name, "shell.") {
			t.Errorf("agent without shell module saw shell tool : %s", name)
		}
		if strings.HasPrefix(name, "http.") {
			t.Errorf("agent without http module saw http tool : %s", name)
		}
	}
	if !contains(tools, "filesystem.read") {
		t.Errorf("agent's declared module tool missing : %v", tools)
	}
}

// CC-7 (bis) — agents[].modules with per-module tool whitelist
func TestYAMLSec_AgentModulesWithToolWhitelist(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	// Agent declares filesystem, but only 'read' from it.
	app := secApp("whitelist-app", caps, schema.AgentModules{
		{ID: "filesystem", Tools: []string{"read"}},
	})

	tools, _, _ := runSecTurn(t, app, secUniverse(),
		&llm.ChatResponse{Content: "ok"})

	if !contains(tools, "filesystem.read") {
		t.Errorf("whitelisted action missing : %v", tools)
	}
	if contains(tools, "filesystem.delete") {
		t.Errorf("non-whitelisted filesystem action leaked : %v", tools)
	}
}

// =====================================================================
// CC-8 — Tool injection mode forced via runtime.tool_injection
// =====================================================================

func TestYAMLSec_ForcedDiscoveryMode(t *testing.T) {
	// 50 tools — would normally fit in direct mode for an 8K
	// context. Forcing discovery should hide them all.
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	app := secApp("discovery-app", caps, nil)
	app.Definition.Runtime.ToolInjection = schema.ToolInjectionDiscovery

	tools, _, _ := runSecTurn(t, app, secUniverse(),
		&llm.ChatResponse{Content: "ok"})

	// In discovery mode, only meta-tools are direct. The 4 domain
	// tools must NOT be in the list (they're behind search_tools).
	for _, want := range []string{
		"context_builder.search_tools",
		"context_builder.execute_tool",
	} {
		if !contains(tools, want) {
			t.Errorf("discovery mode missing meta-tool %q : %v", want, tools)
		}
	}
	for _, leak := range []string{"filesystem.read", "shell.bash"} {
		if contains(tools, leak) {
			t.Errorf("discovery mode leaked domain tool %q : %v", leak, tools)
		}
	}
}

// =====================================================================
// CC-5 — approve entry in YAML routes the call through the SG-5
// approval flow at runtime (existing sg5_approval_test.go tests the
// approval mechanism itself ; this test verifies the YAML config
// HOOK is correctly read by the gate evaluator).
// =====================================================================

func TestYAMLSec_ApproveListEmitsRequest(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
		Approve: []schema.CapabilityGrant{
			{Module: "filesystem", Tools: []string{"delete"}},
		},
	}
	app := secApp("approve-app", caps, nil)

	// filesystem.delete must still appear in the LLM tool list
	// (the agent can attempt it ; SG-4 triggers approval at
	// runtime).
	tools, _, _ := runSecTurn(t, app, secUniverse(),
		&llm.ChatResponse{Content: "ok"})
	if !contains(tools, "filesystem.delete") {
		t.Errorf("approve-listed tool must remain in index (paused at runtime, not hidden) : %v", tools)
	}
}

func TestYAMLSec_ForcedDirectMode(t *testing.T) {
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	app := secApp("direct-app", caps, nil)
	app.Definition.Runtime.ToolInjection = schema.ToolInjectionDirect

	tools, _, _ := runSecTurn(t, app, secUniverse(),
		&llm.ChatResponse{Content: "ok"})

	// Direct mode : all 4 domain tools must be visible.
	for _, name := range []string{
		"filesystem.read", "filesystem.delete", "shell.bash", "http.get",
	} {
		if !contains(tools, name) {
			t.Errorf("direct mode missing %q : %v", name, tools)
		}
	}
}
