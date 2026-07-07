package mode_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/mode"
)

func intptr(i int) *int { return &i }

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}



func TestDefaultModeID(t *testing.T) {
	cases := []struct {
		name  string
		modes map[string]schema.ModeDef
		order []string
		want  string
	}{
		{"empty", nil, nil, ""},
		{"auto wins", map[string]schema.ModeDef{"ask": {}, "auto": {}}, []string{"ask", "auto"}, "auto"},
		{"first declared", map[string]schema.ModeDef{"ask": {}, "plan": {}}, []string{"ask", "plan"}, "ask"},
		{"first declared (other order)", map[string]schema.ModeDef{"ask": {}, "plan": {}}, []string{"plan", "ask"}, "plan"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mode.DefaultModeID(c.modes, c.order); got != c.want {
				t.Errorf("DefaultModeID = %q, want %q", got, c.want)
			}
		})
	}
}



func TestResolve_NoModeIsInert(t *testing.T) {
	def := mode.AppDefaults{MaxTurns: 50, Timeout: 300, BaseBehaviorProfile: "coding"}
	eff := mode.Resolve(nil, nil, "", def)
	if eff.ActiveModeID != "" || eff.Filtered() {
		t.Errorf("no modes should be inert: %+v", eff)
	}
	if eff.MaxTurns != 50 || eff.Timeout != 300 || eff.BehaviorProfile != "coding" {
		t.Errorf("inert turn must carry app defaults: %+v", eff)
	}
}

func TestResolve_UnknownIdFallsBackToInert(t *testing.T) {
	modes := map[string]schema.ModeDef{"ask": {Label: "Ask"}}
	eff := mode.Resolve(modes, []string{"ask"}, "nope", mode.AppDefaults{MaxTurns: 50, Timeout: 300})
	if eff.ActiveModeID != "" {
		t.Errorf("unknown id should resolve to no mode, got %q", eff.ActiveModeID)
	}
}

func TestResolve_OverridesAndFallbacks(t *testing.T) {
	modes := map[string]schema.ModeDef{
		"ask": {
			Label:        "Ask",
			MaxTurns:     intptr(8),
			SystemPrompt: "Read-only.",
			ToolGrants: []schema.CapabilityGrant{
				{Module: "filesystem", Tools: []string{"read"}},
			},
			BehaviorProfile: "assistant",
		},
	}
	def := mode.AppDefaults{MaxTurns: 50, Timeout: 300, BaseBehaviorProfile: "coding"}
	eff := mode.Resolve(modes, []string{"ask"}, "ask", def)

	if eff.ActiveModeID != "ask" || eff.ModeLabel != "Ask" {
		t.Errorf("id/label wrong: %+v", eff)
	}
	if eff.MaxTurns != 8 {
		t.Errorf("MaxTurns = %d, want 8 (mode override)", eff.MaxTurns)
	}
	if eff.Timeout != 300 {
		t.Errorf("Timeout = %v, want 300 (app fallback)", eff.Timeout)
	}
	if eff.SystemPromptSuffix != "Read-only." {
		t.Errorf("suffix lost: %q", eff.SystemPromptSuffix)
	}
	if !eff.Filtered() {
		t.Error("tool_grants present → must filter")
	}
	if eff.BehaviorProfile != "assistant" {
		t.Errorf("BehaviorProfile = %q, want assistant", eff.BehaviorProfile)
	}
}

func TestResolve_LabelDefaultsToCapitalizedId(t *testing.T) {
	modes := map[string]schema.ModeDef{"plan": {}}
	eff := mode.Resolve(modes, []string{"plan"}, "plan", mode.AppDefaults{MaxTurns: 50, Timeout: 300})
	if eff.ModeLabel != "Plan" {
		t.Errorf("label = %q, want Plan (capitalized id)", eff.ModeLabel)
	}
}

func TestResolve_EmptyGrantsInheritAll(t *testing.T) {
	modes := map[string]schema.ModeDef{"auto": {Label: "Auto", ToolGrants: []schema.CapabilityGrant{}}}
	eff := mode.Resolve(modes, []string{"auto"}, "auto", mode.AppDefaults{MaxTurns: 50, Timeout: 300})
	if eff.Filtered() {
		t.Error("empty tool_grants must inherit everything (no filtering)")
	}
}

func TestResolve_ZeroMaxTurnsFallsBack(t *testing.T) {
	// Matches Python truthiness: 0 falls back to the app default.
	modes := map[string]schema.ModeDef{"x": {MaxTurns: intptr(0)}}
	eff := mode.Resolve(modes, []string{"x"}, "x", mode.AppDefaults{MaxTurns: 50, Timeout: 300})
	if eff.MaxTurns != 50 {
		t.Errorf("MaxTurns = %d, want 50 (0 falls back)", eff.MaxTurns)
	}
}


func TestPartition_NilGrantsAllAllowed(t *testing.T) {
	allowed, blocked := mode.ComputeToolPartition(nil, []string{"filesystem.read", "shell.bash"})
	if len(blocked) != 0 || len(allowed) != 2 {
		t.Errorf("nil grants: allowed=%v blocked=%v", keys(allowed), keys(blocked))
	}
}

func TestPartition_ActionScopedAllowList(t *testing.T) {
	grants := []schema.CapabilityGrant{{Module: "filesystem", Tools: []string{"read", "glob"}}}
	offered := []string{"filesystem.read", "filesystem.glob", "filesystem.delete", "shell.bash"}
	allowed, blocked := mode.ComputeToolPartition(grants, offered)

	wantAllowed := []string{"filesystem.glob", "filesystem.read"}
	wantBlocked := []string{"filesystem.delete", "shell.bash"}
	if got := keys(allowed); strings.Join(got, ",") != strings.Join(wantAllowed, ",") {
		t.Errorf("allowed = %v, want %v", got, wantAllowed)
	}
	if got := keys(blocked); strings.Join(got, ",") != strings.Join(wantBlocked, ",") {
		t.Errorf("blocked = %v, want %v", got, wantBlocked)
	}
}

func TestPartition_WholeModuleGrant(t *testing.T) {
	grants := []schema.CapabilityGrant{{Module: "web"}} // no actions = whole module
	allowed, blocked := mode.ComputeToolPartition(grants, []string{"web.search", "web.fetch", "shell.bash"})
	if _, ok := allowed["web.search"]; !ok {
		t.Error("web.search should be allowed (whole module)")
	}
	if _, ok := allowed["web.fetch"]; !ok {
		t.Error("web.fetch should be allowed (whole module)")
	}
	if _, ok := blocked["shell.bash"]; !ok {
		t.Error("shell.bash should be blocked (module not granted)")
	}
}


func TestPartition_MetaToolsAlwaysAllowed(t *testing.T) {
	grants := []schema.CapabilityGrant{{Module: "filesystem", Tools: []string{"read"}}}
	offered := []string{
		"context_builder.execute_tool", "context_builder.search_tools",
		"context_builder.run_parallel", "filesystem.read", "shell.bash",
	}
	allowed, blocked := mode.ComputeToolPartition(grants, offered)
	for _, meta := range []string{"context_builder.execute_tool", "context_builder.search_tools", "context_builder.run_parallel"} {
		if _, ok := allowed[meta]; !ok {
			t.Errorf("%s must always be allowed (infra bypass), got blocked", meta)
		}
	}
	if _, ok := blocked["shell.bash"]; !ok {
		t.Error("shell.bash should still be blocked")
	}
}

func TestPartition_SanitizedNamesPartitionByCanonicalModule(t *testing.T) {

	grants := []schema.CapabilityGrant{{Module: "filesystem", Tools: []string{"read"}}}
	allowed, blocked := mode.ComputeToolPartition(grants, []string{"filesystem__read", "filesystem__delete"})
	if _, ok := allowed["filesystem__read"]; !ok {
		t.Error("filesystem__read should be allowed (canonicalized to filesystem.read)")
	}
	if _, ok := blocked["filesystem__delete"]; !ok {
		t.Error("filesystem__delete should be blocked")
	}
}



func TestSwitchMessage_AllAllowed(t *testing.T) {
	eff := mode.EffectiveTurn{ActiveModeID: "auto", ModeLabel: "Auto"}
	allowed := map[string]struct{}{"filesystem.read": {}}
	msg := mode.BuildModeSwitchMessage(eff, allowed, nil)
	if !strings.HasPrefix(msg, "[Mode: Auto]") {
		t.Errorf("missing header: %q", msg)
	}
	if !strings.Contains(msg, "All tools are available in this mode.") {
		t.Errorf("no-block message wrong: %q", msg)
	}
}

func TestSwitchMessage_WithBlocked(t *testing.T) {
	eff := mode.EffectiveTurn{ActiveModeID: "ask", ModeLabel: "Ask", SystemPromptSuffix: "Read-only investigation."}
	allowed := map[string]struct{}{"filesystem.read": {}, "filesystem.glob": {}}
	blocked := map[string]struct{}{"shell.bash": {}, "filesystem.delete": {}}
	msg := mode.BuildModeSwitchMessage(eff, allowed, blocked)

	for _, want := range []string{
		"[Mode: Ask]",
		"Read-only investigation.",
		"Tools available in this mode: filesystem.glob, filesystem.read",
		"Tools blocked in this mode: filesystem.delete, shell.bash",
		"ask the user to switch",
		"Do not retry a blocked tool.",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("switch message missing %q\n---\n%s", want, msg)
		}
	}
}
