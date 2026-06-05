package behavior

import (
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

func ruleActive(defs []ruleDef, id string) bool {
	for i := range defs {
		if defs[i].id == id {
			return true
		}
	}
	return false
}

func hasViolation(vios []Violation, ruleID, level string) bool {
	for _, v := range vios {
		if v.RuleID == ruleID && (level == "" || v.Level == level) {
			return true
		}
	}
	return false
}

func TestProfiles_SelectRulesByFlag(t *testing.T) {
	coding := New(&schema.BehaviorConfig{Profile: "coding"})
	if !ruleActive(coding.ruleDefs, "read_before_edit") {
		t.Error("coding must enable read_before_edit")
	}
	if !ruleActive(coding.ruleDefs, "confirm_destructive") {
		t.Error("coding must enable confirm_destructive")
	}

	assistant := New(&schema.BehaviorConfig{Profile: "assistant"})
	if ruleActive(assistant.ruleDefs, "search_before_read") {
		t.Error("assistant has search_before_read=false; rule must be inactive")
	}
	if !ruleActive(assistant.ruleDefs, "read_before_edit") {
		t.Error("assistant has read_before_edit=true")
	}
}

func TestReadBeforeEdit_WarnThenClearAfterRead(t *testing.T) {
	be := New(&schema.BehaviorConfig{Profile: "coding"})
	const sid = "s1"
	be.OnTurnStart(sid)

	vios := be.PreTool(sid, "filesystem.edit", map[string]any{"file_path": "/x.go"}, "")
	if !hasViolation(vios, "read_before_edit", "warn") {
		t.Fatalf("editing an unread file must warn; got %+v", vios)
	}

	// Reading the file records it ; a subsequent edit is clean.
	be.PostTool(sid, "filesystem.read", map[string]any{"file_path": "/x.go"}, nil)
	vios = be.PreTool(sid, "filesystem.edit", map[string]any{"file_path": "/x.go"}, "")
	if hasViolation(vios, "read_before_edit", "") {
		t.Fatalf("editing a read file must NOT warn; got %+v", vios)
	}
}

func TestConfirmDestructive_Blocks(t *testing.T) {
	be := New(&schema.BehaviorConfig{Profile: "coding"})
	const sid = "s1"
	be.OnTurnStart(sid)
	vios := be.PreTool(sid, "shell.bash", map[string]any{"command": "rm -rf /tmp/x"}, "")
	if !hasViolation(vios, "confirm_destructive", "block") {
		t.Fatalf("rm -rf must block; got %+v", vios)
	}
}

func TestSearchBeforeRead_ThresholdFromProfile(t *testing.T) {
	// coding: max_blind_reads=3 → the 4th blind read warns.
	be := New(&schema.BehaviorConfig{Profile: "coding"})
	const sid = "s1"
	be.OnTurnStart(sid)
	for i, f := range []string{"/a", "/b", "/c"} {
		vios := be.PreTool(sid, "filesystem.read", map[string]any{"file_path": f}, "")
		if hasViolation(vios, "search_before_read", "") {
			t.Fatalf("read #%d should not warn yet; got %+v", i+1, vios)
		}
		be.PostTool(sid, "filesystem.read", map[string]any{"file_path": f}, nil)
	}
	vios := be.PreTool(sid, "filesystem.read", map[string]any{"file_path": "/d"}, "")
	if !hasViolation(vios, "search_before_read", "warn") {
		t.Fatalf("the 4th blind read must warn (max_blind_reads=3); got %+v", vios)
	}
}

func TestConsecutiveSameTool_LookaheadThreshold(t *testing.T) {
	be := New(&schema.BehaviorConfig{Profile: "coding"}) // max_sequential_same_tool=8
	const sid = "s1"
	be.OnTurnStart(sid)
	args := map[string]any{"command": "echo hi"}
	for i := 1; i <= 7; i++ {
		vios := be.PreTool(sid, "shell.bash", args, "")
		if hasViolation(vios, "max_sequential_same_tool", "") {
			t.Fatalf("call #%d must not warn yet; got %+v", i, vios)
		}
		be.PostTool(sid, "shell.bash", args, nil)
	}
	// 8th pre-check: lookahead = 7+1 = 8 >= 8 → fires.
	vios := be.PreTool(sid, "shell.bash", args, "")
	if !hasViolation(vios, "max_sequential_same_tool", "warn") {
		t.Fatalf("the 8th consecutive call must warn; got %+v", vios)
	}
}

// TestProfileSwap_PreservesStateChangesRules is the doc point-6 contract:
// swapping the active profile re-resolves the rules while per-session
// counters / sets / flags survive.
func TestProfileSwap_PreservesStateChangesRules(t *testing.T) {
	be := New(&schema.BehaviorConfig{Profile: "coding"})
	const sid = "s1"
	be.OnTurnStart(sid)
	for _, f := range []string{"/a", "/b", "/c", "/d"} {
		be.PostTool(sid, "filesystem.read", map[string]any{"file_path": f}, nil)
	}
	st := be.getSession(sid)
	if got := st.counter("reads_since_search"); got != 4 {
		t.Fatalf("reads_since_search = %d, want 4", got)
	}
	if got := st.setLen("read_files"); got != 4 {
		t.Fatalf("read_files size = %d, want 4", got)
	}

	// Swap to research (search_before_read=false, max_blind_reads=10).
	be.SetActiveProfile(sid, "research")

	// State preserved across the swap.
	if got := st.counter("reads_since_search"); got != 4 {
		t.Errorf("counter not preserved across swap: %d, want 4", got)
	}
	if got := st.setLen("read_files"); got != 4 {
		t.Errorf("set not preserved across swap: %d, want 4", got)
	}
	// Rules re-resolved: search_before_read no longer active.
	if ruleActive(be.effectiveRuleDefs(st), "search_before_read") {
		t.Error("after swap to research, search_before_read must be inactive")
	}
	vios := be.PreTool(sid, "filesystem.read", map[string]any{"file_path": "/e"}, "")
	if hasViolation(vios, "search_before_read", "") {
		t.Errorf("research must not warn on blind reads; got %+v", vios)
	}
}

// TestPerSessionProfileIsolation proves the fix for the reference daemon's
// global-clobber bug: one session's profile swap must not change another
// session's active rules.
func TestPerSessionProfileIsolation(t *testing.T) {
	be := New(&schema.BehaviorConfig{Profile: "coding"}) // read_before_edit=true
	be.OnTurnStart("s1")
	be.OnTurnStart("s2")

	// s1 swaps to research (read_before_edit=false).
	be.SetActiveProfile("s1", "research")

	// s2 stays on coding → still enforces read_before_edit.
	v2 := be.PreTool("s2", "filesystem.edit", map[string]any{"file_path": "/x"}, "")
	if !hasViolation(v2, "read_before_edit", "warn") {
		t.Errorf("s2 (coding) must still enforce read_before_edit; got %+v", v2)
	}
	// s1 (research) must NOT enforce it.
	v1 := be.PreTool("s1", "filesystem.edit", map[string]any{"file_path": "/x"}, "")
	if hasViolation(v1, "read_before_edit", "") {
		t.Errorf("s1 (research) must not enforce read_before_edit; got %+v", v1)
	}
}

func TestSetActiveProfile_NoOpAndFallback(t *testing.T) {
	be := New(&schema.BehaviorConfig{Profile: "assistant"})
	const sid = "s1"
	st := be.getSession(sid)

	be.SetActiveProfile(sid, "") // target == "" == activeProfile → no-op
	if st.ruleDefs != nil {
		t.Error(`SetActiveProfile("") on a fresh session must stay on the default (nil ruleDefs)`)
	}

	be.SetActiveProfile(sid, "dev")
	if st.activeProfile != "dev" || st.ruleDefs == nil {
		t.Fatalf("swap to dev must set per-session ruleDefs; activeProfile=%q", st.activeProfile)
	}

	be.SetActiveProfile(sid, "") // empty falls back to YAML default → share engine set
	if st.activeProfile != "" || st.ruleDefs != nil {
		t.Errorf("empty profile must revert to YAML default (nil per-session ruleDefs)")
	}
}

func TestStateTracking_ReadIncrementsGrepResets(t *testing.T) {
	be := New(&schema.BehaviorConfig{Profile: "coding"})
	const sid = "s1"
	be.OnTurnStart(sid)
	be.PostTool(sid, "filesystem.read", map[string]any{"file_path": "/a"}, nil)
	be.PostTool(sid, "filesystem.read", map[string]any{"file_path": "/b"}, nil)
	st := be.getSession(sid)
	if got := st.counter("reads_since_search"); got != 2 {
		t.Fatalf("reads_since_search = %d, want 2", got)
	}
	be.PostTool(sid, "filesystem.grep", map[string]any{"pattern": "foo"}, nil)
	if got := st.counter("reads_since_search"); got != 0 {
		t.Errorf("grep must reset reads_since_search to 0; got %d", got)
	}
	if !st.inSet("searched_patterns", "foo") {
		t.Error("grep must record the searched pattern")
	}
}

func TestPromptText_DevGuideAndRules(t *testing.T) {
	be := New(&schema.BehaviorConfig{Profile: "dev"})
	const sid = "s1"
	txt := be.PromptText(sid)
	if txt == "" {
		t.Fatal("dev profile must produce a prompt section")
	}
	if !contains(txt, "ENFORCED BEHAVIORAL RULES") {
		t.Error("prompt must list enforced rules")
	}
	if !contains(txt, "DEVELOPER BEHAVIOR GUIDE") {
		t.Error("dev profile must inject the developer guide")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
