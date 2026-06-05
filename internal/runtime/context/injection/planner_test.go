package injection_test

import (
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/runtime/context/index"
	"github.com/mbathepaul/digitorn/internal/runtime/context/injection"
	"github.com/mbathepaul/digitorn/internal/runtime/policy"
)

// makeUniverse builds n tools across a few modules with realistic
// descriptions (~30-80 chars) so the token estimator sees data
// representative of real apps.
func makeUniverse(n int) []policy.AvailableAction {
	out := make([]policy.AvailableAction, 0, n)
	modules := []string{"filesystem", "shell", "memory", "web", "http", "database"}
	for i := 0; i < n; i++ {
		mod := modules[i%len(modules)]
		out = append(out, policy.AvailableAction{
			Module: mod,
			Action: actionName(i),
			Spec: &tool.Spec{
				Name:        mod + "." + actionName(i),
				Description: "Operation #" + actionName(i) + " on " + mod + " — does the thing it advertises.",
				RiskLevel:   tool.RiskLow,
				Params: []tool.ParamSpec{
					{Name: "input", Type: "string", Description: "the thing to operate on", Required: true},
				},
			},
		})
	}
	return out
}

func actionName(i int) string {
	letters := []byte("abcdefghijklmnopqrstuvwxyz")
	if i < 26 {
		return string(letters[i])
	}
	// Walk multiple letters for indices >= 26. Handles 1000+ easily.
	var buf []byte
	n := i
	for {
		buf = append([]byte{letters[n%26]}, buf...)
		n = n/26 - 1
		if n < 0 {
			break
		}
	}
	return string(buf)
}

// buildIndex is a test helper : full universe with no restrictions.
func buildIndex(t *testing.T, n int) *index.ToolIndex {
	t.Helper()
	caps := &schema.CapabilitiesConfig{
		DefaultPolicy: schema.CapAuto,
		MaxRiskLevel:  schema.RiskLevel(tool.RiskHigh),
	}
	return index.NewBuilder().Build(true, caps, &schema.Agent{ID: "main"}, makeUniverse(n))
}

// agentWithWindow returns an Agent whose brain context window is set.
func agentWithWindow(window int) *schema.Agent {
	return &schema.Agent{
		ID: "main",
		Brain: schema.Brain{
			Provider: "openai",
			Model:    "gpt-4o-mini",
			Context: &schema.ContextConfig{
				MaxTokens: window,
			},
		},
	}
}

// =====================================================================
// Mode selection (the doc's auto-algorithm)
// =====================================================================

// TestPlan_SmallToolset_ChoosesDirect : a 3-tool app on an 8K window
// has plenty of budget (1600 tokens) and full schemas fit easily →
// direct mode.
func TestPlan_SmallToolset_ChoosesDirect(t *testing.T) {
	idx := buildIndex(t, 3)
	p := &injection.Planner{}
	d := p.Plan(idx, agentWithWindow(8000), nil)
	if d.Mode != injection.ModeDirect {
		t.Fatalf("mode = %v, want direct (budget=%d, tool_tokens=%d)",
			d.Mode, d.Budget, d.ToolTokens)
	}
}

// TestPlan_MediumToolset_ChoosesCompactDirect : 80 tools doesn't fit
// in direct (80 * ~200 fallback = 16K > 1600 budget) BUT compact
// (80 * 30 = 2400) still fits if we have a slightly larger window.
//
// 12K context → budget=2400 → compact (compact_tokens=2400) just
// fits, direct (~~12000) doesn't.
func TestPlan_MediumToolset_ChoosesCompactDirect(t *testing.T) {
	idx := buildIndex(t, 80)
	p := &injection.Planner{}
	d := p.Plan(idx, agentWithWindow(12000), nil)
	if d.Mode != injection.ModeCompactDirect {
		t.Fatalf("mode = %v, want compact_direct (budget=%d, tool_tokens=%d, compact_tokens=%d)",
			d.Mode, d.Budget, d.ToolTokens, d.CompactTokens)
	}
}

// TestPlan_LargeToolset_ChoosesDiscovery : 500 tools doesn't even
// fit compact (500 * 30 = 15K) on an 8K window → discovery.
func TestPlan_LargeToolset_ChoosesDiscovery(t *testing.T) {
	idx := buildIndex(t, 500)
	p := &injection.Planner{}
	d := p.Plan(idx, agentWithWindow(8000), nil)
	if d.Mode != injection.ModeDiscovery {
		t.Fatalf("mode = %v, want discovery (budget=%d, tool_tokens=%d, compact_tokens=%d)",
			d.Mode, d.Budget, d.ToolTokens, d.CompactTokens)
	}
}

// =====================================================================
// Override (YAML runtime.tool_injection)
// =====================================================================

// TestPlan_OverrideForceDirect : even with 1000 tools, override forces direct.
func TestPlan_OverrideForceDirect(t *testing.T) {
	idx := buildIndex(t, 1000)
	rt := &schema.RuntimeBlock{ToolInjection: schema.ToolInjectionDirect}
	p := &injection.Planner{}
	d := p.Plan(idx, agentWithWindow(8000), rt)
	if d.Mode != injection.ModeDirect {
		t.Fatalf("mode = %v, want direct (override)", d.Mode)
	}
	if !d.OverrideUsed {
		t.Error("OverrideUsed should be true")
	}
}

// TestPlan_OverrideForceDiscovery : tiny toolset forced to discovery.
func TestPlan_OverrideForceDiscovery(t *testing.T) {
	idx := buildIndex(t, 3)
	rt := &schema.RuntimeBlock{ToolInjection: schema.ToolInjectionDiscovery}
	p := &injection.Planner{}
	d := p.Plan(idx, agentWithWindow(128000), rt)
	if d.Mode != injection.ModeDiscovery {
		t.Fatalf("mode = %v, want discovery (override)", d.Mode)
	}
}

// =====================================================================
// Output : always-direct builtins + mode-specific domain tools
// =====================================================================

// TestPlan_DirectMode_AppendsDomainTools : direct mode emits the domain tools
// with full schemas + ONLY the run_parallel/background_run execution primitives.
// The discovery / schema-fetch meta-tools are NOT injected in direct mode (every
// tool is already inline → they'd be pure context pollution).
func TestPlan_DirectMode_AppendsDomainTools(t *testing.T) {
	idx := buildIndex(t, 3)
	p := &injection.Planner{}
	d := p.Plan(idx, agentWithWindow(8000), nil)

	if d.Mode != injection.ModeDirect {
		t.Fatalf("setup error : want direct, got %v", d.Mode)
	}

	// The relevant builtins (execution primitives) come first.
	if d.Tools[0].Name != "context_builder__run_parallel" {
		t.Errorf("first tool = %s, want context_builder__run_parallel", d.Tools[0].Name)
	}
	// No discovery meta-tools in direct mode.
	names := map[string]bool{}
	for _, ts := range d.Tools {
		names[ts.Name] = true
	}
	for _, gone := range []string{"context_builder__search_tools", "context_builder__get_tool", "context_builder__execute_tool"} {
		if names[gone] {
			t.Errorf("direct mode must NOT inject discovery meta-tool %q", gone)
		}
	}
	// Total = 2 execution primitives + 3 domain tools = 5.
	if len(d.Tools) != 5 {
		t.Errorf("len(Tools) = %d, want 5 (2 primitives + 3 domain)", len(d.Tools))
	}
	// Direct mode tools carry params. Wire form sanitizes dots → __.
	found := false
	for _, ts := range d.Tools {
		if ts.Name == "filesystem__a" {
			found = true
			if ts.Parameters == nil {
				t.Error("filesystem.a missing Parameters in direct mode")
			}
		}
	}
	if !found {
		t.Error("filesystem.a missing from direct mode output")
	}
}

// TestPlan_CompactDirectMode_DomainToolsHaveNoParams : compact mode
// emits domain tools WITHOUT Parameters → the LLM must call get_tool.
func TestPlan_CompactDirectMode_DomainToolsHaveNoParams(t *testing.T) {
	idx := buildIndex(t, 80)
	p := &injection.Planner{}
	d := p.Plan(idx, agentWithWindow(12000), nil)

	if d.Mode != injection.ModeCompactDirect {
		t.Fatalf("setup : want compact_direct, got %v", d.Mode)
	}
	for _, ts := range d.Tools {
		if ts.Name == "filesystem__a" && ts.Parameters != nil {
			t.Error("compact_direct domain tools must not carry Parameters")
		}
	}
}

// TestPlan_DiscoveryMode_OnlyBuiltins : discovery emits only the
// 10 always-direct builtins ; no domain tools.
func TestPlan_DiscoveryMode_OnlyBuiltins(t *testing.T) {
	idx := buildIndex(t, 500)
	p := &injection.Planner{}
	d := p.Plan(idx, agentWithWindow(8000), nil)

	if d.Mode != injection.ModeDiscovery {
		t.Fatalf("setup : want discovery, got %v", d.Mode)
	}
	if len(d.Tools) != 5 {
		t.Errorf("len(Tools) = %d, want 5 (search_tools+get_tool+execute_tool+run_parallel+background_run)", len(d.Tools))
	}
	const prefix = "context_builder__"
	for _, ts := range d.Tools {
		// Every emitted tool starts with "context_builder__"
		if len(ts.Name) < len(prefix) || ts.Name[:len(prefix)] != prefix {
			t.Errorf("discovery mode emitted non-builtin %q", ts.Name)
		}
	}
}

// TestPlan_BuiltinsRelevantPerMode : the activation-by-relevance policy. Each
// mode injects ONLY the builtins it needs, never the rest (no pollution).
func TestPlan_BuiltinsRelevantPerMode(t *testing.T) {
	idx := buildIndex(t, 3)
	p := &injection.Planner{}
	cases := map[injection.Mode]struct {
		want, absent []string
	}{
		injection.ModeDirect: {
			want:   []string{"context_builder__run_parallel", "context_builder__background_run"},
			absent: []string{"context_builder__search_tools", "context_builder__get_tool", "context_builder__execute_tool"},
		},
		injection.ModeCompactDirect: {
			want:   []string{"context_builder__get_tool", "context_builder__execute_tool", "context_builder__run_parallel", "context_builder__background_run"},
			absent: []string{"context_builder__search_tools"},
		},
		injection.ModeDiscovery: {
			// search_tools is the UNIFIED discovery tool (search + list + browse).
			want:   []string{"context_builder__search_tools", "context_builder__get_tool", "context_builder__execute_tool", "context_builder__run_parallel", "context_builder__background_run"},
			absent: []string{"context_builder__list_categories", "context_builder__browse_category"},
		},
	}
	for mode, exp := range cases {
		rt := &schema.RuntimeBlock{ToolInjection: schema.ToolInjection(mode)}
		d := p.Plan(idx, agentWithWindow(8000), rt)
		names := map[string]bool{}
		for _, ts := range d.Tools {
			names[ts.Name] = true
		}
		for _, w := range exp.want {
			if !names[w] {
				t.Errorf("mode %s : builtin %q missing", mode, w)
			}
		}
		for _, a := range exp.absent {
			if names[a] {
				t.Errorf("mode %s : builtin %q must NOT be injected (pollution)", mode, a)
			}
		}
	}
}

// =====================================================================
// Edge cases
// =====================================================================

// TestPlan_NilIndex_EmptyToolset_NoBuiltins : nil index means 0 tools — a
// pure-chat agent. The activation-by-relevance policy injects NO builtins
// (nothing to discover / run / background). The wiring may still append
// module-gated tools (memory / agent) on top, but the planner adds none.
func TestPlan_NilIndex_EmptyToolset_NoBuiltins(t *testing.T) {
	p := &injection.Planner{}
	d := p.Plan(nil, agentWithWindow(8000), nil)
	if d.Mode != injection.ModeDirect {
		t.Errorf("empty index → got %v, want direct", d.Mode)
	}
	if len(d.Tools) != 0 {
		t.Errorf("empty index (pure chat) : got %d tools, want 0 (no pollution)", len(d.Tools))
	}
}

// TestPlan_NoContextConfig_UsesDefault : no agent.Brain.Context →
// fall back to DefaultContextWindow.
func TestPlan_NoContextConfig_UsesDefault(t *testing.T) {
	idx := buildIndex(t, 3)
	p := &injection.Planner{}
	d := p.Plan(idx, &schema.Agent{ID: "main"}, nil)
	if d.ContextWindow != injection.DefaultContextWindow {
		t.Errorf("ContextWindow = %d, want default %d",
			d.ContextWindow, injection.DefaultContextWindow)
	}
}

// TestPlan_BudgetCalculation_MatchesDoc : on a 100K context window
// with 5 tools at ~300 tokens each, budget=20K, tool_tokens=~1500,
// compact_tokens=150 → direct.
func TestPlan_BudgetCalculation_MatchesDoc(t *testing.T) {
	idx := buildIndex(t, 5)
	p := &injection.Planner{}
	d := p.Plan(idx, agentWithWindow(100000), nil)
	if d.Budget != 20000 {
		t.Errorf("Budget = %d, want 20000 (20%% of 100K)", d.Budget)
	}
	if d.Mode != injection.ModeDirect {
		t.Errorf("Mode = %v, want direct", d.Mode)
	}
}

// TestPlan_CustomContextRatio_Honoured : the Planner's ContextRatio
// override lets tests dial the budget without touching constants.
func TestPlan_CustomContextRatio_Honoured(t *testing.T) {
	idx := buildIndex(t, 100)
	p := &injection.Planner{ContextRatio: 0.10}
	d := p.Plan(idx, agentWithWindow(20000), nil)
	if d.Budget != 2000 {
		t.Errorf("custom ratio not applied : Budget = %d, want 2000", d.Budget)
	}
}

// TestPlan_OverrideReason_Surfaced : when an override is used, the
// reason string mentions the forced mode so an audit log makes it
// clear why the algorithm wasn't used.
func TestPlan_OverrideReason_Surfaced(t *testing.T) {
	idx := buildIndex(t, 3)
	rt := &schema.RuntimeBlock{ToolInjection: schema.ToolInjectionDiscovery}
	p := &injection.Planner{}
	d := p.Plan(idx, agentWithWindow(100000), rt)
	if d.OverrideReason == "" {
		t.Error("OverrideReason should be set when override active")
	}
}

// =====================================================================
// Builtin spec sanity
// =====================================================================

// TestBuiltins_ContainExpectedFields : every builtin has a Name,
// non-empty Description, and a Parameters object. Catches accidental
// removal of a doc-mandated meta-tool.
func TestBuiltins_AreWellFormed(t *testing.T) {
	idx := buildIndex(t, 1)
	rt := &schema.RuntimeBlock{ToolInjection: schema.ToolInjectionDiscovery}
	p := &injection.Planner{}
	d := p.Plan(idx, agentWithWindow(100000), rt)

	for _, ts := range d.Tools {
		if ts.Name == "" {
			t.Errorf("tool with empty Name : %+v", ts)
		}
		if ts.Description == "" {
			t.Errorf("tool %s : empty Description", ts.Name)
		}
		if ts.Parameters == nil {
			t.Errorf("tool %s : nil Parameters (LLM provider rejects)", ts.Name)
		}
	}
}
