package behavior

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

func toStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	case bool:
		if x {
			return "True"
		}
		return "False"
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case int:
		return strconv.Itoa(x)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func intFromAny(v any, def int) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return def
}

func evaluateCondition(cond map[string]any, st *SessionState, toolName string, params map[string]any, result any, agentText string, tk *tracking) bool {
	if len(cond) == 0 {
		return true
	}

	if raw, ok := cond["target_not_in_set"]; ok {
		name, _ := raw.(string)
		target := extractTarget(params, setCfgFor(tk, name))
		return target != "" && !st.inSet(name, target)
	}
	if raw, ok := cond["target_in_set"]; ok {
		name, _ := raw.(string)
		target := extractTarget(params, setCfgFor(tk, name))
		return target != "" && st.inSet(name, target)
	}
	if raw, ok := cond["counter_gte"]; ok {
		cfg, _ := raw.(map[string]any)
		name, _ := cfg["name"].(string)
		return st.counter(name) >= intFromAny(cfg["value"], 0)
	}
	if raw, ok := cond["param_matches"]; ok {
		cfg, _ := raw.(map[string]any)
		pname, _ := cfg["param"].(string)
		pattern, _ := cfg["pattern"].(string)
		val := paramString(params, pname)
		if pattern == "" || val == "" {
			return false
		}
		return matchRegex(pattern, val)
	}
	if raw, ok := cond["param_contains"]; ok {
		cfg, _ := raw.(map[string]any)
		pname, _ := cfg["param"].(string)
		value, _ := cfg["value"].(string)
		return strings.Contains(strings.ToLower(paramString(params, pname)), strings.ToLower(value))
	}
	if raw, ok := cond["param_has_any"]; ok {
		names, _ := raw.([]any)
		for _, n := range names {
			if name, ok := n.(string); ok {
				if _, exists := params[name]; exists {
					return true
				}
			}
		}
		return false
	}
	if raw, ok := cond["flag_is"]; ok {
		cfg, _ := raw.(map[string]any)
		name, _ := cfg["name"].(string)
		want := true
		if b, ok := cfg["value"].(bool); ok {
			want = b
		}
		return st.flag(name) == want
	}
	if b, ok := cond["no_text_before_tools"].(bool); ok && b {
		return !st.PlanStated
	}
	if b, ok := cond["first_tool_this_turn"].(bool); ok && b {
		return st.ToolCallsThisTurn == 0
	}
	if raw, ok := cond["consecutive_gte"]; ok {
		threshold := intFromAny(raw, 0)
		upcoming := 1
		if strings.EqualFold(toolName, st.LastToolName) {
			upcoming = st.ConsecutiveSame + 1
		}
		return upcoming >= threshold
	}
	if raw, ok := cond["tool_calls_this_turn_eq"]; ok {
		return st.ToolCallsThisTurn == intFromAny(raw, -1)
	}
	if b, ok := cond["target_exists_on_disk"].(bool); ok && b {
		target := extractTarget(params, nil)
		if target == "" {
			return false
		}
		_, err := os.Stat(target)
		return err == nil
	}
	if raw, ok := cond["text_matches"]; ok {
		pattern, _ := raw.(string)
		return matchRegex(pattern, agentText)
	}
	if b, ok := cond["result_has_lint_errors"].(bool); ok && b {
		return resultHasLintErrors(result)
	}

	if raw, ok := cond["all"]; ok {
		for _, c := range asCondList(raw) {
			if !evaluateCondition(c, st, toolName, params, result, agentText, tk) {
				return false
			}
		}
		return true
	}
	if raw, ok := cond["any"]; ok {
		for _, c := range asCondList(raw) {
			if evaluateCondition(c, st, toolName, params, result, agentText, tk) {
				return true
			}
		}
		return false
	}
	if raw, ok := cond["not"]; ok {
		if sub, ok := raw.(map[string]any); ok {
			return !evaluateCondition(sub, st, toolName, params, result, agentText, tk)
		}
	}
	return false
}

func asCondList(raw any) []map[string]any {
	list, _ := raw.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func setCfgFor(tk *tracking, name string) *setCfg {
	if tk == nil {
		return nil
	}
	if c, ok := tk.sets[name]; ok {
		return &c
	}
	return nil
}

func resultHasLintErrors(result any) bool {
	m, ok := result.(map[string]any)
	if !ok {
		return false
	}
	lint, ok := m["lint"]
	if !ok {
		if data, ok := m["data"].(map[string]any); ok {
			lint = data["lint"]
		}
	}
	items, ok := lint.([]any)
	if !ok {
		return false
	}
	for _, it := range items {
		if d, ok := it.(map[string]any); ok {
			if sev, _ := d["severity"].(string); sev == "error" {
				return true
			}
		}
	}
	return false
}

func updateState(st *SessionState, toolName string, params map[string]any, tk *tracking) {
	tn := strings.ToLower(toolName)
	b := bare(tn)
	matches := func(list []string) bool {
		for _, t := range list {
			tl := strings.ToLower(t)
			if tn == tl || b == tl || b == bare(tl) {
				return true
			}
		}
		return false
	}

	st.onToolCall(toolName, extractTarget(params, nil))

	for name, cfg := range tk.sets {
		if matches(cfg.addOn) {
			c := cfg
			if target := extractTarget(params, &c); target != "" {
				st.addToSet(name, target)
			}
		}
	}
	for name, cfg := range tk.counters {
		if matches(cfg.incrementOn) {
			st.incrCounter(name)
		}
		if matches(cfg.resetOn) {
			st.resetCounter(name)
		}
		if rw := cfg.resetWhen; rw != nil {
			rwTools := strings.Split(strings.ToLower(rw.tool), ",")
			for i := range rwTools {
				rwTools[i] = strings.TrimSpace(rwTools[i])
			}
			if matches(rwTools) {
				if rw.matches != "" && matchRegex(rw.matches, paramString(params, rw.param)) {
					st.resetCounter(name)
				}
			}
		}
	}
	for name, cfg := range tk.flags {
		if matches(cfg.setOn) {
			st.setFlag(name, true)
		}
		if matches(cfg.unsetOn) {
			st.setFlag(name, false)
		}
	}
}

var (
	rePlaceholderParam    = regexp.MustCompile(`\{param:(\w+)\}`)
	rePlaceholderCounter  = regexp.MustCompile(`\{counter:(\w+)\}`)
	rePlaceholderSetCount = regexp.MustCompile(`\{set_count:(\w+)\}`)
	rePlaceholderFlag     = regexp.MustCompile(`\{flag:(\w+)\}`)
	reGroupName           = regexp.MustCompile(`\w+`)
)

func renderMessage(template string, st *SessionState, toolName string, params map[string]any) string {
	if template == "" {
		return ""
	}
	target := extractTarget(params, nil)
	msg := template
	msg = strings.ReplaceAll(msg, "{target}", target)
	msg = strings.ReplaceAll(msg, "{tool}", bare(toolName))
	msg = strings.ReplaceAll(msg, "{turn}", strconv.Itoa(st.Turn))
	msg = strings.ReplaceAll(msg, "{tool_calls_this_turn}", strconv.Itoa(st.ToolCallsThisTurn))
	msg = strings.ReplaceAll(msg, "{consecutive_same_tool}", strconv.Itoa(st.ConsecutiveSame))

	msg = rePlaceholderParam.ReplaceAllStringFunc(msg, func(m string) string {
		name := reGroupName.FindString(m[len("{param:"):])
		v := paramString(params, name)
		if len(v) > 100 {
			v = v[:97] + "..."
		}
		return v
	})
	msg = rePlaceholderCounter.ReplaceAllStringFunc(msg, func(m string) string {
		name := reGroupName.FindString(m[len("{counter:"):])
		return strconv.Itoa(st.counter(name))
	})
	msg = rePlaceholderSetCount.ReplaceAllStringFunc(msg, func(m string) string {
		name := reGroupName.FindString(m[len("{set_count:"):])
		return strconv.Itoa(st.setLen(name))
	})
	msg = rePlaceholderFlag.ReplaceAllStringFunc(msg, func(m string) string {
		name := reGroupName.FindString(m[len("{flag:"):])
		if st.flag(name) {
			return "True"
		}
		return "False"
	})
	return msg
}

type Violation struct {
	RuleID  string
	Level   string
	Message string
}

func (v Violation) Format() string {
	switch v.Level {
	case "block":
		return "[BEHAVIOR BLOCKED] " + v.Message + "\nRule: " + v.RuleID +
			"\nThe tool call was NOT executed. Fix the violation first."
	case "warn":
		return "[BEHAVIOR WARNING] " + v.Message + "\nRule: " + v.RuleID
	default:
		return "[BEHAVIOR REMINDER] " + v.Message + "\nRule: " + v.RuleID
	}
}

func checkRules(ruleDefs []ruleDef, st *SessionState, toolName string, params map[string]any, when string, result any, agentText string, tk *tracking) []Violation {
	var out []Violation
	for i := range ruleDefs {
		r := &ruleDefs[i]
		if r.when != when {
			continue
		}
		if !toolMatches(toolName, r.trigger) {
			continue
		}
		if evaluateCondition(r.condition, st, toolName, params, result, agentText, tk) {
			msg := r.message
			if msg == "" {
				msg = r.description
			}
			out = append(out, Violation{
				RuleID:  r.id,
				Level:   r.action,
				Message: renderMessage(msg, st, toolName, params),
			})
		}
	}
	return out
}

func buildPromptSection(ruleDefs []ruleDef) string {
	if len(ruleDefs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("The following rules are ACTIVELY ENFORCED at runtime. Violations are detected and signaled immediately.\n\n")
	for i := range ruleDefs {
		r := &ruleDefs[i]
		desc := r.description
		if desc == "" {
			if r.message != "" {
				desc = r.message
			} else {
				desc = r.id
			}
		}
		tag := "enforced"
		if r.action == "block" {
			tag = "BLOCKED"
		}
		fmt.Fprintf(&b, "%d. %s (%s)\n", i+1, desc, tag)
	}
	return strings.TrimRight(b.String(), "\n")
}
