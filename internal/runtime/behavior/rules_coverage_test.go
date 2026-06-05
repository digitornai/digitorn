package behavior

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

// TestCondition_AllPrimitives exercises every condition primitive + the
// all/any/not composites, both the firing and non-firing branch, by calling
// evaluateCondition directly. This is the exhaustive proof that each primitive
// behaves exactly like the reference evaluator.
func TestCondition_AllPrimitives(t *testing.T) {
	tk := defaultStateTracking
	tmp := t.TempDir()
	existing := filepath.Join(tmp, "exists.txt")
	if err := os.WriteFile(existing, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		cond      map[string]any
		setup     func(*SessionState)
		tool      string
		params    map[string]any
		result    any
		agentText string
		want      bool
	}{
		{"target_not_in_set/true", map[string]any{"target_not_in_set": "read_files"}, nil, "read", map[string]any{"file_path": "/a"}, nil, "", true},
		{"target_not_in_set/false", map[string]any{"target_not_in_set": "read_files"}, func(s *SessionState) { s.addToSet("read_files", "/a") }, "read", map[string]any{"file_path": "/a"}, nil, "", false},
		{"target_in_set/true", map[string]any{"target_in_set": "read_files"}, func(s *SessionState) { s.addToSet("read_files", "/a") }, "read", map[string]any{"file_path": "/a"}, nil, "", true},
		{"target_in_set/false", map[string]any{"target_in_set": "read_files"}, nil, "read", map[string]any{"file_path": "/a"}, nil, "", false},
		{"counter_gte/true", map[string]any{"counter_gte": map[string]any{"name": "c", "value": 3}}, func(s *SessionState) { s.counters["c"] = 3 }, "x", nil, nil, "", true},
		{"counter_gte/false", map[string]any{"counter_gte": map[string]any{"name": "c", "value": 3}}, func(s *SessionState) { s.counters["c"] = 2 }, "x", nil, nil, "", false},
		{"param_matches/true", map[string]any{"param_matches": map[string]any{"param": "command", "pattern": `rm\s+-rf`}}, nil, "bash", map[string]any{"command": "rm -rf /tmp"}, nil, "", true},
		{"param_matches/false", map[string]any{"param_matches": map[string]any{"param": "command", "pattern": `rm\s+-rf`}}, nil, "bash", map[string]any{"command": "ls -la"}, nil, "", false},
		{"param_contains/true", map[string]any{"param_contains": map[string]any{"param": "command", "value": "DANGER"}}, nil, "bash", map[string]any{"command": "this is dangerous"}, nil, "", true},
		{"param_contains/false", map[string]any{"param_contains": map[string]any{"param": "command", "value": "DANGER"}}, nil, "bash", map[string]any{"command": "safe"}, nil, "", false},
		{"flag_is/true", map[string]any{"flag_is": map[string]any{"name": "f", "value": true}}, func(s *SessionState) { s.setFlag("f", true) }, "x", nil, nil, "", true},
		{"flag_is/false-on-unset", map[string]any{"flag_is": map[string]any{"name": "f", "value": false}}, nil, "x", nil, nil, "", true},
		{"flag_is/mismatch", map[string]any{"flag_is": map[string]any{"name": "f", "value": true}}, nil, "x", nil, nil, "", false},
		{"no_text_before_tools/true", map[string]any{"no_text_before_tools": true}, nil, "x", nil, nil, "", true},
		{"no_text_before_tools/false", map[string]any{"no_text_before_tools": true}, func(s *SessionState) { s.PlanStated = true }, "x", nil, nil, "", false},
		{"first_tool_this_turn/true", map[string]any{"first_tool_this_turn": true}, nil, "x", nil, nil, "", true},
		{"first_tool_this_turn/false", map[string]any{"first_tool_this_turn": true}, func(s *SessionState) { s.ToolCallsThisTurn = 1 }, "x", nil, nil, "", false},
		{"consecutive_gte/true", map[string]any{"consecutive_gte": 3}, func(s *SessionState) { s.LastToolName = "read"; s.ConsecutiveSame = 2 }, "read", nil, nil, "", true},
		{"consecutive_gte/false", map[string]any{"consecutive_gte": 3}, func(s *SessionState) { s.LastToolName = "read"; s.ConsecutiveSame = 1 }, "read", nil, nil, "", false},
		{"tool_calls_this_turn_eq/true", map[string]any{"tool_calls_this_turn_eq": 2}, func(s *SessionState) { s.ToolCallsThisTurn = 2 }, "x", nil, nil, "", true},
		{"tool_calls_this_turn_eq/false", map[string]any{"tool_calls_this_turn_eq": 2}, func(s *SessionState) { s.ToolCallsThisTurn = 3 }, "x", nil, nil, "", false},
		{"target_exists_on_disk/true", map[string]any{"target_exists_on_disk": true}, nil, "write", map[string]any{"file_path": existing}, nil, "", true},
		{"target_exists_on_disk/false", map[string]any{"target_exists_on_disk": true}, nil, "write", map[string]any{"file_path": filepath.Join(tmp, "nope.txt")}, nil, "", false},
		{"text_matches/true", map[string]any{"text_matches": `not sure`}, nil, "*", nil, nil, "I'm not sure about this", true},
		{"text_matches/false", map[string]any{"text_matches": `not sure`}, nil, "*", nil, nil, "all good", false},
		{"result_has_lint_errors/true", map[string]any{"result_has_lint_errors": true}, nil, "edit", nil, map[string]any{"lint": []any{map[string]any{"severity": "error"}}}, "", true},
		{"result_has_lint_errors/nested-data", map[string]any{"result_has_lint_errors": true}, nil, "edit", nil, map[string]any{"data": map[string]any{"lint": []any{map[string]any{"severity": "error"}}}}, "", true},
		{"result_has_lint_errors/warn-only", map[string]any{"result_has_lint_errors": true}, nil, "edit", nil, map[string]any{"lint": []any{map[string]any{"severity": "warning"}}}, "", false},
		{"all/true", map[string]any{"all": []any{map[string]any{"first_tool_this_turn": true}, map[string]any{"no_text_before_tools": true}}}, nil, "x", nil, nil, "", true},
		{"all/false", map[string]any{"all": []any{map[string]any{"first_tool_this_turn": true}, map[string]any{"flag_is": map[string]any{"name": "f", "value": true}}}}, nil, "x", nil, nil, "", false},
		{"any/true", map[string]any{"any": []any{map[string]any{"flag_is": map[string]any{"name": "f", "value": true}}, map[string]any{"first_tool_this_turn": true}}}, nil, "x", nil, nil, "", true},
		{"any/false", map[string]any{"any": []any{map[string]any{"flag_is": map[string]any{"name": "f", "value": true}}, map[string]any{"first_tool_this_turn": false}}}, func(s *SessionState) { s.ToolCallsThisTurn = 1 }, "x", nil, nil, "", false},
		{"not/true", map[string]any{"not": map[string]any{"flag_is": map[string]any{"name": "f", "value": true}}}, nil, "x", nil, nil, "", true},
		{"not/false", map[string]any{"not": map[string]any{"flag_is": map[string]any{"name": "f", "value": true}}}, func(s *SessionState) { s.setFlag("f", true) }, "x", nil, nil, "", false},
		{"empty/always-true", map[string]any{}, nil, "x", nil, nil, "", true},
		{"unknown/false", map[string]any{"bogus_primitive": true}, nil, "x", nil, nil, "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newSessionState()
			if c.setup != nil {
				c.setup(st)
			}
			got := evaluateCondition(c.cond, st, c.tool, c.params, c.result, c.agentText, tk)
			if got != c.want {
				t.Errorf("evaluateCondition(%s) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

// TestDefaultRules_EachFires drives every one of the 14 built-in rules under
// the dev profile (which enables all of them) to its firing condition and
// asserts the violation + level. This is the exhaustive proof that each
// shipped rule actually triggers through the engine.
func TestDefaultRules_EachFires(t *testing.T) {
	newDev := func() (*Engine, string) {
		be := New(&schema.BehaviorConfig{Profile: "dev"})
		const sid = "s"
		be.OnTurnStart(sid)
		return be, sid
	}

	t.Run("read_before_edit/warn", func(t *testing.T) {
		be, sid := newDev()
		if !hasViolation(be.PreTool(sid, "filesystem.edit", map[string]any{"file_path": "/x"}, "x"), "read_before_edit", "warn") {
			t.Error("edit on unread file must warn")
		}
	})

	t.Run("read_before_write_existing/warn", func(t *testing.T) {
		be, sid := newDev()
		tmp := t.TempDir()
		f := filepath.Join(tmp, "e.txt")
		os.WriteFile(f, []byte("x"), 0o644)
		// agentText non-empty avoids plan_before_execute noise ; we only check ours.
		if !hasViolation(be.PreTool(sid, "filesystem.write", map[string]any{"file_path": f}, "x"), "read_before_write_existing", "warn") {
			t.Error("write to an existing unread file must warn")
		}
	})

	t.Run("search_before_read/warn", func(t *testing.T) {
		be, sid := newDev() // dev: max_blind_reads=2
		be.PostTool(sid, "filesystem.read", map[string]any{"file_path": "/a"}, nil)
		be.PostTool(sid, "filesystem.read", map[string]any{"file_path": "/b"}, nil)
		if !hasViolation(be.PreTool(sid, "filesystem.read", map[string]any{"file_path": "/c"}, "x"), "search_before_read", "warn") {
			t.Error("3rd blind read must warn under dev (max_blind_reads=2)")
		}
	})

	t.Run("no_bash_for_files/warn", func(t *testing.T) {
		be, sid := newDev()
		if !hasViolation(be.PreTool(sid, "shell.bash", map[string]any{"command": "cat /etc/hosts"}, "x"), "no_bash_for_files", "warn") {
			t.Error("bash cat must warn")
		}
	})

	t.Run("no_blind_exploration/warn", func(t *testing.T) {
		be, sid := newDev()
		if !hasViolation(be.PreTool(sid, "shell.bash", map[string]any{"command": "find . -name x"}, "x"), "no_blind_exploration", "warn") {
			t.Error("bash find must warn")
		}
	})

	t.Run("confirm_destructive/block", func(t *testing.T) {
		be, sid := newDev()
		if !hasViolation(be.PreTool(sid, "shell.bash", map[string]any{"command": "rm -rf /tmp/x"}, "x"), "confirm_destructive", "block") {
			t.Error("rm -rf must block")
		}
	})

	t.Run("plan_before_execute/warn", func(t *testing.T) {
		be, sid := newDev() // plan_stated false, first tool of the turn
		if !hasViolation(be.PreTool(sid, "filesystem.ls", map[string]any{"path": "/"}, ""), "plan_before_execute", "warn") {
			t.Error("first tool with no prior plan text must warn")
		}
	})

	t.Run("verify_after_edit/remind", func(t *testing.T) {
		be, sid := newDev()
		if !hasViolation(be.PostTool(sid, "filesystem.edit", map[string]any{"file_path": "/x"}, nil), "verify_after_edit", "remind") {
			t.Error("post-edit must remind to verify")
		}
	})

	t.Run("test_after_changes/remind", func(t *testing.T) {
		be, sid := newDev() // dev: changes_before_test_reminder=2
		be.PostTool(sid, "filesystem.edit", map[string]any{"file_path": "/a"}, nil)
		if !hasViolation(be.PostTool(sid, "filesystem.write", map[string]any{"file_path": "/b"}, nil), "test_after_changes", "remind") {
			t.Error("2 changes without a test must remind")
		}
	})

	t.Run("delegate_complex/remind", func(t *testing.T) {
		be, sid := newDev()
		var last []Violation
		for i := 0; i < 8; i++ {
			last = be.PostTool(sid, "filesystem.ls", map[string]any{"path": "/"}, nil)
		}
		if !hasViolation(last, "delegate_complex", "remind") {
			t.Error("8 tool calls in a turn must remind to delegate")
		}
	})

	t.Run("delegate_large_reads/remind", func(t *testing.T) {
		be, sid := newDev()
		var last []Violation
		for i := 0; i < 5; i++ {
			last = be.PostTool(sid, "filesystem.read", map[string]any{"file_path": "/f"}, nil)
		}
		if !hasViolation(last, "delegate_large_reads", "remind") {
			t.Error("5 sequential reads must remind to delegate")
		}
	})

	t.Run("web_search_when_unknown/warn", func(t *testing.T) {
		be, sid := newDev()
		if !hasViolation(be.OnAgentText(sid, "Honestly I'm not sure how this works"), "web_search_when_unknown", "warn") {
			t.Error("uncertainty text without a prior web search must warn")
		}
	})

	t.Run("always_lint_check/warn", func(t *testing.T) {
		be, sid := newDev()
		res := map[string]any{"lint": []any{map[string]any{"severity": "error"}}}
		if !hasViolation(be.PostTool(sid, "filesystem.edit", map[string]any{"file_path": "/x"}, res), "always_lint_check", "warn") {
			t.Error("lint errors after edit must warn")
		}
	})

	t.Run("max_sequential_same_tool/warn", func(t *testing.T) {
		be, sid := newDev() // dev threshold 8
		args := map[string]any{"path": "/"}
		for i := 1; i <= 7; i++ {
			be.PreTool(sid, "filesystem.ls", args, "x")
			be.PostTool(sid, "filesystem.ls", args, nil)
		}
		if !hasViolation(be.PreTool(sid, "filesystem.ls", args, "x"), "max_sequential_same_tool", "warn") {
			t.Error("8th consecutive same-tool call must warn")
		}
	})
}

// TestActions_AllThreeLevels confirms each action level renders the expected
// directive prefix and (for block) the non-execution wording.
func TestActions_AllThreeLevels(t *testing.T) {
	cases := []struct {
		level  string
		prefix string
	}{
		{"block", "[BEHAVIOR BLOCKED]"},
		{"warn", "[BEHAVIOR WARNING]"},
		{"remind", "[BEHAVIOR REMINDER]"},
	}
	for _, c := range cases {
		v := Violation{RuleID: "r", Level: c.level, Message: "m"}
		out := v.Format()
		if !contains(out, c.prefix) {
			t.Errorf("level %q must render prefix %q, got %q", c.level, c.prefix, out)
		}
	}
	if !contains(Violation{Level: "block", Message: "m", RuleID: "r"}.Format(), "NOT executed") {
		t.Error("block must state the tool was not executed")
	}
}
