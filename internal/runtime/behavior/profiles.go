package behavior

import (
	"encoding/json"
	"strings"
)

// profiles are the built-in presets. A profile is a flat bag of boolean rule
// flags + integer thresholds + two string knobs (verbosity, autonomy). It is a
// SELECTOR, not a rule list : each true flag pulls the matching rule out of
// defaultRuleDefinitions (see boolToRuleID).
var profiles = map[string]map[string]any{
	"dev": {
		"read_before_edit":             true,
		"read_before_write_existing":   true,
		"search_before_read":           true,
		"test_after_changes":           true,
		"verify_after_edit":            true,
		"plan_before_execute":          true,
		"no_bash_for_files":            true,
		"no_blind_exploration":         true,
		"confirm_complex_plans":        true,
		"confirm_destructive":          true,
		"delegate_complex":             true,
		"delegate_large_reads":         true,
		"web_search_when_unknown":      true,
		"always_lint_check":            true,
		"max_blind_reads":              2,
		"changes_before_test_reminder": 2,
		"max_sequential_same_tool":     8,
		"verbosity":                    "concise",
		"autonomy":                     "medium",
	},
	"coding": {
		"read_before_edit":             true,
		"read_before_write_existing":   true,
		"search_before_read":           true,
		"test_after_changes":           true,
		"verify_after_edit":            true,
		"no_bash_for_files":            true,
		"no_blind_exploration":         true,
		"confirm_complex_plans":        true,
		"confirm_destructive":          true,
		"delegate_complex":             true,
		"delegate_large_reads":         true,
		"web_search_when_unknown":      false,
		"plan_before_execute":          true,
		"always_lint_check":            true,
		"max_blind_reads":              3,
		"changes_before_test_reminder": 3,
		"max_sequential_same_tool":     8,
		"verbosity":                    "concise",
		"autonomy":                     "high",
	},
	"research": {
		"read_before_edit":             false,
		"search_before_read":           false,
		"test_after_changes":           false,
		"verify_after_edit":            false,
		"no_bash_for_files":            true,
		"no_blind_exploration":         true,
		"confirm_complex_plans":        false,
		"confirm_destructive":          true,
		"delegate_complex":             true,
		"delegate_large_reads":         false,
		"web_search_when_unknown":      true,
		"plan_before_execute":          true,
		"always_lint_check":            false,
		"max_blind_reads":              10,
		"changes_before_test_reminder": 99,
		"max_sequential_same_tool":     15,
		"verbosity":                    "detailed",
		"autonomy":                     "high",
	},
	"data": {
		"read_before_edit":             true,
		"read_before_write_existing":   true,
		"search_before_read":           false,
		"test_after_changes":           true,
		"verify_after_edit":            true,
		"no_bash_for_files":            true,
		"no_blind_exploration":         true,
		"confirm_complex_plans":        true,
		"confirm_destructive":          true,
		"delegate_complex":             true,
		"delegate_large_reads":         true,
		"web_search_when_unknown":      true,
		"plan_before_execute":          true,
		"always_lint_check":            true,
		"max_blind_reads":              5,
		"changes_before_test_reminder": 3,
		"max_sequential_same_tool":     10,
		"verbosity":                    "normal",
		"autonomy":                     "medium",
	},
	"creative": {
		"read_before_edit":             true,
		"search_before_read":           false,
		"test_after_changes":           false,
		"verify_after_edit":            false,
		"no_bash_for_files":            true,
		"no_blind_exploration":         true,
		"confirm_complex_plans":        true,
		"confirm_destructive":          true,
		"delegate_complex":             false,
		"delegate_large_reads":         false,
		"web_search_when_unknown":      true,
		"plan_before_execute":          true,
		"always_lint_check":            true,
		"max_blind_reads":              10,
		"changes_before_test_reminder": 99,
		"max_sequential_same_tool":     15,
		"verbosity":                    "detailed",
		"autonomy":                     "low",
	},
	"assistant": {
		"read_before_edit":             true,
		"read_before_write_existing":   true,
		"search_before_read":           false,
		"test_after_changes":           false,
		"verify_after_edit":            false,
		"no_bash_for_files":            true,
		"no_blind_exploration":         true,
		"confirm_complex_plans":        false,
		"confirm_destructive":          true,
		"delegate_complex":             false,
		"delegate_large_reads":         false,
		"web_search_when_unknown":      true,
		"plan_before_execute":          false,
		"always_lint_check":            false,
		"max_blind_reads":              10,
		"changes_before_test_reminder": 99,
		"max_sequential_same_tool":     10,
		"verbosity":                    "normal",
		"autonomy":                     "medium",
	},
}

// resolveProfile merges a profile preset with user overrides. A profileName
// beginning with "{" is a JSON custom profile (from a behavior/*.yaml bundle):
// it carries an `extends` base, extra `rules`, legacy `custom` rules and a
// free-text `prompt`. Reserved keys (prefixed "_") carry resolved metadata.
func resolveProfile(profileName string, overrides map[string]any) map[string]any {
	var custom map[string]any
	if strings.HasPrefix(profileName, "{") {
		_ = json.Unmarshal([]byte(profileName), &custom)
	}

	base := map[string]any{}
	if custom != nil {
		extends, _ := custom["extends"].(string)
		for k, v := range profiles[extends] {
			base[k] = v
		}
		if rules, ok := custom["rules"].(map[string]any); ok {
			for k, v := range rules {
				base[k] = v
			}
		}
		if cl, ok := custom["custom"].([]any); ok {
			base["custom"] = appendCustom(base["custom"], cl)
		}
		if p, ok := custom["prompt"].(string); ok && p != "" {
			base["_custom_prompt"] = p
		}
		if n, ok := custom["name"].(string); ok && n != "" {
			base["_profile_display_name"] = n
		}
		if d, ok := custom["description"].(string); ok && d != "" {
			base["_profile_description"] = d
		}
		name := extends
		if name == "" {
			if n, ok := custom["name"].(string); ok {
				name = n
			} else {
				name = "custom"
			}
		}
		base["_profile_name"] = name
	} else {
		for k, v := range profiles[profileName] {
			base[k] = v
		}
		base["_profile_name"] = profileName
	}

	for k, v := range overrides {
		base[k] = v
	}
	return base
}

func appendCustom(existing any, extra []any) []any {
	out, _ := existing.([]any)
	return append(out, extra...)
}

// truthy mirrors the reference daemon's `if merged.get(key)` flag check, which
// relies on Python truthiness. It matters because some enable-flags double as
// thresholds (e.g. max_sequential_same_tool is the int 8, truthy → enabled).
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case string:
		return x != ""
	default:
		return true
	}
}

// intFlag reads a profile integer threshold, accepting the int / int64 /
// float64 forms YAML and JSON decode into. Falls back to def when absent.
func intFlag(rules map[string]any, key string, def int) int {
	switch v := rules[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return def
}

// devPromptSection is the advanced dev-profile behavior guide, injected into
// the system prompt when the active profile is "dev".
const devPromptSection = `You are guided by the "dev" behavioral profile - the highest standard of developer behavior. Follow these principles in every situation.

## How you think

1. UNDERSTAND before acting. When you receive a task, STOP. Ask yourself:
   - What is the user actually trying to achieve?
   - How big is this task? (1 file? 5 files? entire module?)
   - Do I understand the codebase well enough to do this safely?
   - What could go wrong?

2. PLAN before coding. For any task touching 3+ files:
   - Explore the codebase first (Glob for structure, Grep for patterns)
   - Identify ALL files that need changes
   - Write a numbered plan with specific file paths and what changes each needs
   - Present the plan to the user with AskUser and WAIT for approval
   - Only start coding after the user says yes

3. SIMPLIFY. Try the simplest approach first:
   - One Grep often answers the question. Don't read 10 files when 1 search works.
   - A 3-line fix is better than a 50-line refactor.
   - If you can answer in 2-3 tool calls, do that.

## How you explore code

For small questions (where is X defined?):
  -> Grep('pattern') -> Read the matching section -> Answer

For medium questions (how does module X work?):
  -> Glob('module/**/*.py') to see structure
  -> Grep('class|def') for overview
  -> Read key files (entry point, main class)
  -> Answer with file paths and line numbers

For large exploration (analyze entire codebase):
  -> DELEGATE to sub-agents. Launch 2-3 explore agents in parallel.
  -> Collect results, synthesize, then answer

NEVER read files one by one when you can parallelize or delegate. Your context window is precious - protect it.

## How you implement changes

### Small change (1-2 files, clear fix):
1. Read the file (or relevant section)
2. Edit with exact old_string
3. Read back to verify
4. Run tests if available

### Medium change (3-5 files):
1. Grep to find all locations that need changes
2. Present a plan
3. Wait for user approval
4. Implement file by file, reading before each edit
5. Verify each edit by reading back
6. Run tests after all changes

### Large change (5+ files, new feature, refactoring):
1. Explore the codebase thoroughly (delegate to explore agents if needed)
2. Create a detailed plan (files, order, risks, rollback)
3. Present with AskUser and wait for approval
4. Implement in phases: core -> integration -> tests
5. Run tests after each phase
6. Report progress with tasks

### Critical changes (migrations, security, config):
-> ALWAYS ask before touching. Use AskUser with a clear explanation.

## How you verify

After EVERY edit:
  -> Read the modified section back. Did it come out right?
  -> Check the lint field in the result. Errors? Fix immediately.

After implementing a feature:
  -> Run the test suite. If no tests exist, say so explicitly. Don't claim success without verification.

Before answering a question about code:
  -> ALWAYS read the actual code. Never answer from memory alone.

## How you handle uncertainty

When you don't know something:
  -> Search the web. Don't say "I think" or "probably". Either you know or you search.

When the requirements are ambiguous:
  -> Ask the user. Don't guess and implement the wrong thing.

When something fails:
  -> Read the error carefully. Diagnose before retrying.
  -> After 2 failed attempts, try a completely different approach.

## How you communicate

- State your plan BEFORE calling tools (the user sees your text, not tool params)
- Report what you found AFTER tool calls
- Be concise: lead with the answer, not the reasoning
- Reference code with file_path:line_number so the user can follow`
