package hooks

import (
	"regexp"
	"strings"
	"sync"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

// reCache memoises compiled hook-condition regexes. Patterns come from the
// app's YAML (a bounded, validated set), so the cache is bounded — but
// condition evaluation is on the per-event hot path (HK-4 targets 10M
// sessions), where re-compiling the same pattern on every fire was pure waste.
// Both successes and compile errors are cached so a bad pattern isn't retried.
var reCache sync.Map // pattern string → compiledRe

type compiledRe struct {
	re  *regexp.Regexp
	err error
}

func cachedRegexp(pat string) (*regexp.Regexp, error) {
	if v, ok := reCache.Load(pat); ok {
		c := v.(compiledRe)
		return c.re, c.err
	}
	re, err := regexp.Compile(pat)
	reCache.Store(pat, compiledRe{re: re, err: err})
	return re, err
}

// EvalCondition evaluates a HookCondition against a Payload per
// docs-site/language/31-tool-hooks.md "Conditions (14 built-in)".
//
// The 14 supported types :
//
//	always, never,
//	context_pressure, turn_count, tool_calls, message_count,
//	tool_name, tool_failed,
//	content_contains, error_type, expression,
//	all_of (alias all), any_of (alias any), not
//
// Unknown / unsupported condition types default to FALSE (safe
// fallback ; the compiler validates types separately).
func EvalCondition(cond schema.HookCondition, p Payload) bool {
	switch cond.Type {
	case "", "always":
		return true
	case "never":
		return false

	case "context_pressure":
		return condContextPressure(cond.Params, p)
	case "turn_count":
		return condTurnCount(cond.Params, p)
	case "tool_calls":
		return condTrivialThreshold(cond.Params, p.ToolCallsUsed)
	case "message_count":
		return condTrivialThreshold(cond.Params, p.MessageCount)

	case schema.CondToolName, "tool_match":
		return condToolName(cond.Params, p.ToolName)
	case schema.CondToolFailed:
		return p.ToolStatus == "errored"

	case "content_contains":
		return condContentContains(cond.Params, p)
	case "error_type":
		return condErrorType(cond.Params, p.ErrorType)

	case "expression":
		return condExpression(cond.Params, p)

	case schema.CondAllOf, "all":
		return condComposite(cond.Params, p, true)
	case schema.CondAnyOf, "any":
		return condComposite(cond.Params, p, false)
	case schema.CondNot:
		return !condNot(cond.Params, p)
	}
	return false
}

// =====================================================================
// turn_count / message_count / tool_calls / context_pressure
// =====================================================================

// condContextPressure : fires when TokensUsed / MaxTokens > threshold.
// threshold is REQUIRED — the catalog enforces it at compile-time and the
// auto_compact injection always supplies runtime.context.compression_trigger.
// No value is hardcoded here: a hook reaching this point without a (positive)
// threshold, or before any budget is known, simply never fires.
func condContextPressure(params map[string]any, p Payload) bool {
	if _, ok := params["threshold"]; !ok {
		return false
	}
	threshold := readFloat(params, "threshold", 0)
	if threshold <= 0 || p.MaxTokens <= 0 {
		return false
	}
	ratio := float64(p.TokensUsed) / float64(p.MaxTokens)
	return ratio > threshold
}

// condTurnCount : fires AT or EVERY N turns per doc.
// Params : threshold (required), every (optional).
//
//	"every" set → fires every (current % every == 0) AND
//	              current >= threshold.
//	"every" unset → fires when current == threshold.
func condTurnCount(params map[string]any, p Payload) bool {
	threshold := readInt(params, "threshold", -1)
	if threshold < 0 {
		return false
	}
	every := readInt(params, "every", 0)
	if every > 0 {
		return p.TurnCount >= threshold && p.TurnCount%every == 0
	}
	return p.TurnCount == threshold
}

// condTrivialThreshold : value > threshold. Used by tool_calls and
// message_count which share the same `threshold: int` shape.
func condTrivialThreshold(params map[string]any, current int) bool {
	threshold := readInt(params, "threshold", -1)
	if threshold < 0 {
		return false
	}
	return current >= threshold
}

// =====================================================================
// tool_name / tool_match
// =====================================================================

// condToolName : `match` accepts string OR list[str]. Doc says
// fnmatch glob (NOT regex). Pipe alternation `a|b` supported.
func condToolName(params map[string]any, toolName string) bool {
	if toolName == "" {
		return false
	}
	switch v := params["match"].(type) {
	case string:
		return globMatch(v, toolName)
	case []string:
		for _, s := range v {
			if globMatch(s, toolName) {
				return true
			}
		}
	case []any:
		for _, raw := range v {
			if s, ok := raw.(string); ok && globMatch(s, toolName) {
				return true
			}
		}
	}
	// `tools` is the alias for tool_match.
	switch v := params["tools"].(type) {
	case []string:
		for _, s := range v {
			if globMatch(s, toolName) {
				return true
			}
		}
	case []any:
		for _, raw := range v {
			if s, ok := raw.(string); ok && globMatch(s, toolName) {
				return true
			}
		}
	}
	return false
}

// globMatch : fnmatch-style + pipe alternation. Recognised
// wildcards : * (any chars), ? (one char). Pipe `a|b` matches if
// any sub-pattern matches.
func globMatch(pattern, s string) bool {
	if pattern == "" {
		return false
	}
	if strings.Contains(pattern, "|") {
		for _, p := range strings.Split(pattern, "|") {
			if globMatch(strings.TrimSpace(p), s) {
				return true
			}
		}
		return false
	}
	return fnmatch(pattern, s)
}

// fnmatch is a minimal `*`/`?` matcher for tool names. Tool names
// only use letters/digits/dots/underscores so we don't need [ ]
// character classes. Pure-Go, no allocations on the hot path.
func fnmatch(pattern, s string) bool {
	pi, si := 0, 0
	starPi, starSi := -1, -1
	for si < len(s) {
		if pi < len(pattern) {
			pc := pattern[pi]
			if pc == '?' {
				pi++
				si++
				continue
			}
			if pc == '*' {
				starPi = pi
				starSi = si
				pi++
				continue
			}
			if pc == s[si] {
				pi++
				si++
				continue
			}
		}
		if starPi != -1 {
			pi = starPi + 1
			starSi++
			si = starSi
			continue
		}
		return false
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

// =====================================================================
// content_contains
// =====================================================================

// condContentContains : doc says it matches the LLM's response OR
// the user's message. Param : `keyword: str`.
func condContentContains(params map[string]any, p Payload) bool {
	keyword, _ := params["keyword"].(string)
	if keyword == "" {
		return false
	}
	return strings.Contains(p.LLMContent, keyword) ||
		strings.Contains(p.UserMessage, keyword)
}

// =====================================================================
// error_type
// =====================================================================

// condErrorType : regex against the error type/message. Use with
// the `error` event. Param : `match: str` (regex).
func condErrorType(params map[string]any, errType string) bool {
	pat, _ := params["match"].(string)
	if pat == "" || errType == "" {
		return false
	}
	re, err := cachedRegexp(pat)
	if err != nil {
		return false
	}
	return re.MatchString(errType)
}

// =====================================================================
// expression
// =====================================================================

// condExpression : doc-defined "Python-like expression against the
// turn state". V1 implements a SAFE subset :
//
//   - `tokens_used > N`     → numeric compare
//   - `messages > N`        → numeric compare
//   - `tool_calls > N`      → numeric compare
//   - `tool_failed`         → bool literal (true when status="errored")
//   - "true" / "false"      → bool literals
//
// Unknown / unsafe expressions return false (defensive default).
// Production deployments should prefer the typed conditions
// (context_pressure, turn_count, tool_calls, message_count,
// tool_failed) which cover the common cases without
// expression-language risk.
func condExpression(params map[string]any, p Payload) bool {
	expr, _ := params["expr"].(string)
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return false
	}
	switch expr {
	case "true":
		return true
	case "false":
		return false
	case "tool_failed":
		return p.ToolStatus == "errored"
	}
	if v, ok := evalNumericCompare(expr, p); ok {
		return v
	}
	return false
}

// evalNumericCompare parses "<var> <op> <num>" where op ∈ {>, >=,
// <, <=, ==}. Variables : tokens_used | max_tokens | messages |
// tool_calls | turn_count | open_tasks.
func evalNumericCompare(expr string, p Payload) (bool, bool) {
	for _, op := range []string{">=", "<=", "==", ">", "<"} {
		idx := strings.Index(expr, op)
		if idx <= 0 {
			continue
		}
		left := strings.TrimSpace(expr[:idx])
		right := strings.TrimSpace(expr[idx+len(op):])
		ln, lok := varToFloat(left, p)
		rn, rok := parseFloat(right)
		if !lok || !rok {
			return false, false
		}
		switch op {
		case ">":
			return ln > rn, true
		case ">=":
			return ln >= rn, true
		case "<":
			return ln < rn, true
		case "<=":
			return ln <= rn, true
		case "==":
			return ln == rn, true
		}
	}
	return false, false
}

func varToFloat(name string, p Payload) (float64, bool) {
	switch name {
	case "tokens_used":
		return float64(p.TokensUsed), true
	case "max_tokens":
		return float64(p.MaxTokens), true
	case "messages", "message_count":
		return float64(p.MessageCount), true
	case "tool_calls":
		return float64(p.ToolCallsUsed), true
	case "turn_count", "turns":
		return float64(p.TurnCount), true
	case "open_tasks":
		return float64(p.OpenTasks), true
	}
	return 0, false
}

// =====================================================================
// all_of / any_of / not
// =====================================================================

// condComposite evaluates the `conditions` list with AND (when
// requireAll=true) or OR semantics. Short-circuits on first
// decisive result, matching the doc-defined behaviour
// (31-tool-hooks.md "Composite conditions - short-circuit").
func condComposite(params map[string]any, p Payload, requireAll bool) bool {
	conds := readNestedConditions(params)
	if len(conds) == 0 {
		return requireAll
	}
	for _, c := range conds {
		ok := EvalCondition(c, p)
		if requireAll && !ok {
			return false
		}
		if !requireAll && ok {
			return true
		}
	}
	return requireAll
}

// condNot supports the doc-defined shape :
//
//	not:
//	  condition: { type: tool_failed }
//
// AND a more relaxed multi-form :
//
//	not:
//	  conditions: [ {type: x}, {type: y} ]   # negate ANY match
//
// Returns the *underlying* condition's value ; caller negates.
func condNot(params map[string]any, p Payload) bool {
	if c, ok := readSingleCondition(params); ok {
		return EvalCondition(c, p)
	}
	conds := readNestedConditions(params)
	for _, c := range conds {
		if EvalCondition(c, p) {
			return true
		}
	}
	return false
}

// readSingleCondition reads a `condition:` sub-map (the doc-form
// for `not`).
func readSingleCondition(params map[string]any) (schema.HookCondition, bool) {
	raw, ok := params["condition"]
	if !ok {
		return schema.HookCondition{}, false
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return schema.HookCondition{}, false
	}
	return buildCondFromMap(m), true
}

// readNestedConditions reads the `conditions` list for all_of /
// any_of / not (multi-form).
func readNestedConditions(params map[string]any) []schema.HookCondition {
	raw, ok := params["conditions"]
	if !ok {
		return nil
	}
	var out []schema.HookCondition
	switch v := raw.(type) {
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, buildCondFromMap(m))
			}
		}
	case []map[string]any:
		for _, m := range v {
			out = append(out, buildCondFromMap(m))
		}
	}
	return out
}

// buildCondFromMap turns a YAML-decoded condition map back into
// a schema.HookCondition. `type` becomes Type ; remaining keys
// become Params.
func buildCondFromMap(m map[string]any) schema.HookCondition {
	t, _ := m["type"].(string)
	params := make(map[string]any, len(m))
	for k, v := range m {
		if k == "type" {
			continue
		}
		params[k] = v
	}
	return schema.HookCondition{
		Type:   schema.HookConditionType(t),
		Params: params,
	}
}

// =====================================================================
// helpers
// =====================================================================

func readFloat(params map[string]any, key string, def float64) float64 {
	if v, ok := params[key].(float64); ok {
		return v
	}
	if v, ok := params[key].(int); ok {
		return float64(v)
	}
	return def
}

func readInt(params map[string]any, key string, def int) int {
	switch v := params[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return def
}

func parseFloat(s string) (float64, bool) {
	var f float64
	var neg bool
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	dot := -1
	for i, r := range s {
		if r == '.' {
			if dot != -1 {
				return 0, false
			}
			dot = i
			continue
		}
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	if s == "" {
		return 0, false
	}
	// Simple decimal parse without strconv (avoid import cycle
	// concerns and keep allocations zero).
	if dot == -1 {
		var n int64
		for _, r := range s {
			n = n*10 + int64(r-'0')
		}
		f = float64(n)
	} else {
		intPart := s[:dot]
		fracPart := s[dot+1:]
		var n int64
		for _, r := range intPart {
			n = n*10 + int64(r-'0')
		}
		f = float64(n)
		var frac float64
		var mul float64 = 0.1
		for _, r := range fracPart {
			frac += float64(r-'0') * mul
			mul /= 10
		}
		f += frac
	}
	if neg {
		f = -f
	}
	return f, true
}
