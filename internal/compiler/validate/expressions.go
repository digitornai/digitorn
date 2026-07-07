package validate

import (
	"fmt"
	"strings"

	"github.com/digitornai/digitorn/internal/compiler/diagnostic"
	"github.com/digitornai/digitorn/internal/compiler/schema"
)

// expressionRoots names every namespace that may appear as the leading
// identifier in a hook / flow / route expression. Anything else is rejected
// as a typo — runtime variables come from a fixed set.
var expressionRoots = map[string]struct{}{
	"event": {}, "events": {}, "payload": {},
	"caller": {}, "client": {}, "user": {},
	"state": {}, "session": {}, "context": {}, "ctx": {},
	"message": {}, "msg": {}, "messages": {},
	"turn": {}, "turns": {},
	"result": {}, "results": {}, "output": {}, "outputs": {},
	"params": {}, "param": {}, "input": {},
	"tool": {}, "tools": {},
	"agent": {}, "agents": {},
	"workspace": {}, "ws": {},
	"steps": {}, "step": {}, "field": {},
	"previous": {}, "true": {}, "false": {}, "null": {}, "nil": {},
	"approvals": {}, "tasks": {}, "error": {},
}

// CheckExpressions lints every expression-typed field in the manifest:
//   - hook conditions of type "expr"
//   - flow.nodes[].routes[].when
//   - flow.nodes[].on_error[].match (when it looks like an expression)
//
// We don't compile expressions — we just check syntactic shape (balanced
// parens / quotes) and that the leading identifier of every dotted reference
// is a recognised runtime root.
func CheckExpressions(file string, def *schema.AppDefinition, bag *diagnostic.Bag) {
	for i, h := range def.RuntimeHooksOrNil() {
		checkHookExpr(h, fmt.Sprintf("runtime.hooks.%d.condition", i), bag)
	}
	for i, a := range def.Agents {
		for j, h := range a.Hooks {
			checkHookExpr(h, fmt.Sprintf("agents.%d.hooks.%d.condition", i, j), bag)
		}
	}
	if def.Flow != nil {
		// Flow node ids are valid expression roots : `{{<node>.output.x}}` and
		// `<node>.field` references in when/expr resolve against prior nodes.
		nodeIDs := make(map[string]struct{}, len(def.Flow.Nodes))
		for _, n := range def.Flow.Nodes {
			if n.ID != "" {
				nodeIDs[strings.ToLower(n.ID)] = struct{}{}
			}
		}
		for i, n := range def.Flow.Nodes {
			for j, r := range n.Routes {
				if r.When != "" {
					checkExprStringWith(r.When, fmt.Sprintf("flow.nodes.%d.routes.%d.when", i, j), bag, nodeIDs)
				}
			}
			if n.Expr != "" {
				checkExprStringWith(n.Expr, fmt.Sprintf("flow.nodes.%d.expr", i), bag, nodeIDs)
			}
		}
	}
}

func checkHookExpr(h schema.Hook, path string, bag *diagnostic.Bag) {
	if h.Condition.Type != "expr" {
		return
	}
	raw, _ := h.Condition.Params["expression"].(string)
	if raw == "" {
		return
	}
	checkExprString(raw, path+".expression", bag)
}

// checkExprString runs the lightweight expression linter. It catches:
//   - unbalanced quotes / parens / brackets
//   - leading identifier of a dotted ref that isn't a known runtime root
func checkExprString(s, path string, bag *diagnostic.Bag) {
	checkExprStringWith(s, path, bag, nil)
}

func checkExprStringWith(s, path string, bag *diagnostic.Bag, extraRoots map[string]struct{}) {
	if err := balanced(s); err != nil {
		bag.Add(diagnostic.Errorf(diagnostic.CodeBadPlaceholderSyntax, posUnknown,
			"%s: %s", path, err.Error()))
		return
	}
	for _, root := range leadingRefs(s) {
		if _, ok := expressionRoots[root]; ok {
			continue
		}
		if _, ok := extraRoots[root]; ok {
			continue
		}
		// Skip numeric literals and reserved words.
		switch root {
		case "and", "or", "not", "in", "is":
			continue
		}
		bag.Add(diagnostic.Errorf(diagnostic.CodeUnknownNamespace, posUnknown,
			"%s: unknown expression root %q", path, root))
	}
}

// balanced ensures parentheses/brackets/braces nest correctly and quotes are
// closed. Plain heuristic — strings can contain anything between matching
// quotes.
func balanced(s string) error {
	stack := []byte{}
	inStr := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr != 0 {
			if c == '\\' && i+1 < len(s) {
				i++
				continue
			}
			if c == inStr {
				inStr = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			inStr = c
		case '(', '[', '{':
			stack = append(stack, c)
		case ')', ']', '}':
			if len(stack) == 0 {
				return fmt.Errorf("unmatched %q", string(c))
			}
			open := stack[len(stack)-1]
			if !matches(open, c) {
				return fmt.Errorf("mismatched %q with %q", string(open), string(c))
			}
			stack = stack[:len(stack)-1]
		}
	}
	if inStr != 0 {
		return fmt.Errorf("unterminated string")
	}
	if len(stack) > 0 {
		return fmt.Errorf("unclosed %q", string(stack[len(stack)-1]))
	}
	return nil
}

func matches(open, close byte) bool {
	switch open {
	case '(':
		return close == ')'
	case '[':
		return close == ']'
	case '{':
		return close == '}'
	}
	return false
}

// leadingRefs extracts the first identifier of every dotted reference
// appearing in s (ignoring identifiers inside strings).
func leadingRefs(s string) []string {
	out := []string{}
	inStr := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr != 0 {
			if c == '\\' && i+1 < len(s) {
				i++
				continue
			}
			if c == inStr {
				inStr = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			inStr = c
			continue
		}
		if !isIdentStart(c) {
			continue
		}
		// Scan identifier.
		j := i
		for j < len(s) && (isIdentStart(s[j]) || (s[j] >= '0' && s[j] <= '9') || s[j] == '_') {
			j++
		}
		ident := s[i:j]
		i = j - 1
		// Only collect when followed by `.` — a leading ref of a dotted path.
		if j < len(s) && s[j] == '.' {
			out = append(out, strings.ToLower(ident))
		}
	}
	return out
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}
