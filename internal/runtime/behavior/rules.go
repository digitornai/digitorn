package behavior

import (
	"regexp"
	"strings"
	"sync"
)

// ruleDef is one declarative rule. trigger nil/empty (or containing "*") means
// "all tools". condition is the same nested shape the reference daemon uses
// (a single primitive, or an all/any/not composite) — kept as a map so the
// 13 primitives + composites port faithfully without a bespoke AST.
type ruleDef struct {
	id          string
	description string
	when        string // pre_tool | post_tool | on_text
	action      string // block | warn | remind
	message     string
	trigger     []string
	condition   map[string]any
}

type tracking struct {
	sets     map[string]setCfg
	counters map[string]counterCfg
	flags    map[string]flagCfg
}

type setCfg struct {
	addOn   []string
	target  string
	aliases []string
}

type counterCfg struct {
	incrementOn []string
	resetOn     []string
	resetWhen   *resetWhen
}

type resetWhen struct {
	tool    string
	param   string
	matches string
}

type flagCfg struct {
	setOn   []string
	unsetOn []string
}

var defaultStateTracking = &tracking{
	sets: map[string]setCfg{
		"read_files":        {addOn: []string{"read", "filesystem.read", "filesystem__read"}, target: "file_path", aliases: []string{"path", "filepath"}},
		"edited_files":      {addOn: []string{"edit", "filesystem.edit", "filesystem__edit"}, target: "file_path", aliases: []string{"path", "filepath"}},
		"written_files":     {addOn: []string{"write", "filesystem.write", "filesystem__write"}, target: "file_path", aliases: []string{"path", "filepath"}},
		"searched_patterns": {addOn: []string{"grep", "glob", "filesystem.grep", "filesystem.glob", "filesystem__grep", "filesystem__glob"}, target: "pattern"},
	},
	counters: map[string]counterCfg{
		"reads_since_search": {
			incrementOn: []string{"read", "filesystem.read", "filesystem__read"},
			resetOn:     []string{"grep", "glob", "filesystem.grep", "filesystem.glob", "filesystem__grep", "filesystem__glob"},
		},
		"changes_since_test": {
			incrementOn: []string{"edit", "write", "filesystem.edit", "filesystem.write", "filesystem__edit", "filesystem__write"},
			resetWhen: &resetWhen{
				tool:    "bash,shell.bash,shell__bash",
				param:   "command",
				matches: `\b(pytest|py\.test|npm\s+test|npx\s+jest|cargo\s+test|go\s+test|make\s+test|python\s+-m\s+pytest|vitest|mocha|rspec|phpunit)\b`,
			},
		},
	},
	flags: map[string]flagCfg{
		"has_web_searched": {setOn: []string{"search", "web.search", "web__search"}},
	},
}

func defaultRuleDefinitions() []ruleDef {
	return []ruleDef{
		{
			id: "read_before_edit", action: "warn", when: "pre_tool",
			description: "ALWAYS Read a file before Edit - edit on unread files is flagged",
			trigger:     []string{"edit", "filesystem.edit", "filesystem__edit"},
			condition:   map[string]any{"target_not_in_set": "read_files"},
			message:     "You are editing '{target}' without reading it first. Read it before editing.",
		},
		{
			id: "read_before_write_existing", action: "warn", when: "pre_tool",
			description: "Read existing files before Write - avoids losing content",
			trigger:     []string{"write", "filesystem.write", "filesystem__write"},
			condition: map[string]any{"all": []any{
				map[string]any{"target_not_in_set": "read_files"},
				map[string]any{"target_exists_on_disk": true},
			}},
			message: "'{target}' already exists. Read it first or use Edit for changes.",
		},
		{
			id: "search_before_read", action: "warn", when: "pre_tool",
			description: "Grep/Glob before Read - don't read files blindly",
			trigger:     []string{"read", "filesystem.read", "filesystem__read"},
			condition: map[string]any{"all": []any{
				map[string]any{"target_not_in_set": "read_files"},
				map[string]any{"counter_gte": map[string]any{"name": "reads_since_search", "value": 3}},
			}},
			message: "You have read {counter:reads_since_search} files without searching. Use Grep or Glob first.",
		},
		{
			id: "no_bash_for_files", action: "warn", when: "pre_tool",
			description: "NEVER Bash for file ops (cat/sed/find) - use Read/Edit/Grep/Glob",
			trigger:     []string{"bash", "shell.bash", "shell__bash"},
			condition: map[string]any{"param_matches": map[string]any{
				"param":   "command",
				"pattern": `\b(cat|head|tail|less|more|bat)\s|\bsed\s+-|sed\s+'|\bawk\s|perl\s+-[pi]`,
			}},
			message: "Don't use Bash for file operations: '{param:command}'. Use Read/Edit instead.",
		},
		{
			id: "no_blind_exploration", action: "warn", when: "pre_tool",
			description: "NEVER Bash to explore (find/ls -la/tree) - use Glob",
			trigger:     []string{"bash", "shell.bash", "shell__bash"},
			condition: map[string]any{"param_matches": map[string]any{
				"param":   "command",
				"pattern": `(find\s+\.|ls\s+-[lRa]|ls\s+\.|tree\b|dir\s+/[sS])`,
			}},
			message: "Don't use Bash to explore. Use Glob for structure, Grep for content.",
		},
		{
			id: "confirm_destructive", action: "block", when: "pre_tool",
			description: "Destructive commands are BLOCKED - ask user first",
			trigger:     []string{"bash", "shell.bash", "shell__bash"},
			condition: map[string]any{"param_matches": map[string]any{
				"param":   "command",
				"pattern": `(rm\s+-rf|git\s+reset\s+--hard|git\s+push\s+--force|git\s+push\s+-f\b|drop\s+table|drop\s+database|truncate\s+table|git\s+clean\s+-fd)`,
			}},
			message: "Destructive command detected: '{param:command}'. Ask user confirmation first.",
		},
		{
			id: "plan_before_execute", action: "warn", when: "pre_tool",
			description: "State your plan in text before tools",
			condition: map[string]any{"all": []any{
				map[string]any{"no_text_before_tools": true},
				map[string]any{"first_tool_this_turn": true},
			}},
			message: "State your plan before calling tools. The user cannot see tool parameters.",
		},
		{
			id: "verify_after_edit", action: "remind", when: "post_tool",
			description: "Re-read the modified section after Edit",
			trigger:     []string{"edit", "filesystem.edit", "filesystem__edit"},
			condition:   map[string]any{},
			message:     "You just edited '{target}'. Read back the modified section to verify.",
		},
		{
			id: "test_after_changes", action: "remind", when: "post_tool",
			description: "Run tests after N changes",
			trigger:     []string{"edit", "write", "filesystem.edit", "filesystem.write", "filesystem__edit", "filesystem__write"},
			condition:   map[string]any{"counter_gte": map[string]any{"name": "changes_since_test", "value": 3}},
			message:     "You have made {counter:changes_since_test} changes since last test. Run tests now.",
		},
		{
			id: "delegate_complex", action: "remind", when: "post_tool",
			description: "Delegate to sub-agents when too many tool calls in one turn",
			condition:   map[string]any{"tool_calls_this_turn_eq": 8},
			message:     "You have made {tool_calls_this_turn} tool calls this turn. Delegate to sub-agents.",
		},
		{
			id: "delegate_large_reads", action: "remind", when: "post_tool",
			description: "Delegate bulk reading to sub-agents",
			trigger:     []string{"read", "filesystem.read", "filesystem__read"},
			condition:   map[string]any{"counter_gte": map[string]any{"name": "reads_since_search", "value": 5}},
			message:     "You're reading many files sequentially. Delegate bulk exploration to a sub-agent.",
		},
		{
			id: "web_search_when_unknown", action: "warn", when: "on_text",
			description: "Search the web when expressing uncertainty",
			condition: map[string]any{"all": []any{
				map[string]any{"text_matches": `\b(not sure|I'm unsure|don't know|uncertain|can't remember)\b`},
				map[string]any{"flag_is": map[string]any{"name": "has_web_searched", "value": false}},
			}},
			message: "You expressed uncertainty. Search the web instead of guessing.",
		},
		{
			id: "always_lint_check", action: "warn", when: "post_tool",
			description: "Check lint after Edit/Write",
			trigger:     []string{"edit", "write", "filesystem.edit", "filesystem.write", "filesystem__edit", "filesystem__write"},
			condition:   map[string]any{"result_has_lint_errors": true},
			message:     "Lint found errors in your last change. Fix them before moving on.",
		},
		{
			id: "max_sequential_same_tool", action: "warn", when: "pre_tool",
			description: "Don't repeat the same tool too many times",
			condition:   map[string]any{"consecutive_gte": 8},
			message:     "You have called '{tool}' {consecutive_same_tool} times in a row. Try a different approach.",
		},
	}
}

// boolToRuleID maps a profile boolean flag to the default rule it selects.
var boolToRuleID = map[string]string{
	"read_before_edit":           "read_before_edit",
	"read_before_write_existing": "read_before_write_existing",
	"search_before_read":         "search_before_read",
	"no_bash_for_files":          "no_bash_for_files",
	"no_blind_exploration":       "no_blind_exploration",
	"confirm_destructive":        "confirm_destructive",
	"plan_before_execute":        "plan_before_execute",
	"verify_after_edit":          "verify_after_edit",
	"test_after_changes":         "test_after_changes",
	"delegate_complex":           "delegate_complex",
	"delegate_large_reads":       "delegate_large_reads",
	"web_search_when_unknown":    "web_search_when_unknown",
	"always_lint_check":          "always_lint_check",
	"max_sequential_same_tool":   "max_sequential_same_tool",
}

// boolFlagOrder fixes the iteration order over boolToRuleID so the assembled
// rule list (and the prompt section built from it) is deterministic.
var boolFlagOrder = []string{
	"read_before_edit", "read_before_write_existing", "search_before_read",
	"no_bash_for_files", "no_blind_exploration", "confirm_destructive",
	"plan_before_execute", "verify_after_edit", "test_after_changes",
	"delegate_complex", "delegate_large_reads", "web_search_when_unknown",
	"always_lint_check", "max_sequential_same_tool",
}

func bare(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

func toolMatches(toolName string, triggers []string) bool {
	if len(triggers) == 0 {
		return true
	}
	tn := strings.ToLower(toolName)
	b := bare(tn)
	for _, t := range triggers {
		if t == "*" {
			return true
		}
		tl := strings.ToLower(t)
		if tn == tl || b == tl || b == bare(tl) {
			return true
		}
	}
	return false
}

func paramString(params map[string]any, key string) string {
	if params == nil {
		return ""
	}
	v, ok := params[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return toStr(v)
}

func extractTarget(params map[string]any, cfg *setCfg) string {
	if cfg != nil {
		tp := cfg.target
		if tp == "" {
			tp = "file_path"
		}
		if v := paramString(params, tp); v != "" {
			return v
		}
		for _, a := range cfg.aliases {
			if v := paramString(params, a); v != "" {
				return v
			}
		}
	}
	for _, key := range []string{"file_path", "path", "filepath", "url", "query", "pattern", "target"} {
		if v := paramString(params, key); v != "" {
			return v
		}
	}
	return ""
}

var (
	reMu    sync.Mutex
	reCache = map[string]*regexp.Regexp{}
)

// matchRegex compiles (case-insensitive, cached) and tests pattern against s.
// A bad pattern matches nothing — mirrors the reference's swallowed re.error.
func matchRegex(pattern, s string) bool {
	if pattern == "" {
		return false
	}
	reMu.Lock()
	re, ok := reCache[pattern]
	if !ok {
		compiled, err := regexp.Compile("(?i)" + pattern)
		if err != nil {
			reCache[pattern] = nil
		} else {
			reCache[pattern] = compiled
		}
		re = reCache[pattern]
	}
	reMu.Unlock()
	if re == nil {
		return false
	}
	return re.MatchString(s)
}
