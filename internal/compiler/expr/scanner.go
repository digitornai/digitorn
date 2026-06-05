package expr

import (
	"errors"
	"fmt"
	"strings"
)

// ResolveString scans s for {{...}} placeholders and substitutes each with
// the engine's evaluation. Resolution is recursive: if a substituted value
// itself contains new placeholders, they are resolved up to MaxDepth.
//
// A placeholder that resolves to a literal copy of itself ({{X}} → {{X}}) is
// recognised as a runtime passthrough and is not recursed on, so the depth
// counter only fires for genuine cycles.
func (e *Engine) ResolveString(s string) (string, error) {
	return e.resolveDepth(s, 0)
}

func (e *Engine) resolveDepth(s string, depth int) (string, error) {
	if depth >= e.maxDepth {
		return "", fmt.Errorf("placeholder resolution exceeded max depth %d (cycle?)", e.maxDepth)
	}
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	var b strings.Builder
	i := 0
	for i < len(s) {
		open := strings.Index(s[i:], "{{")
		if open < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i : i+open])
		close := strings.Index(s[i+open+2:], "}}")
		if close < 0 {
			return "", fmt.Errorf("unterminated placeholder at offset %d", i+open)
		}
		body := s[i+open+2 : i+open+2+close]
		original := s[i+open : i+open+2+close+2]
		// Nested resolution (depth > 0) means we're walking content that was
		// itself returned by a previous resolution — a prompt file, a skill,
		// or a variable's value. Anything unparseable there is documentation
		// text, not a real placeholder, and we leave it literal.
		expr, err := Parse(body)
		if err != nil {
			if depth > 0 {
				b.WriteString(original)
				i += open + 2 + close + 2
				continue
			}
			return "", fmt.Errorf("{{%s}}: %w", body, err)
		}
		val, err := e.Eval(expr)
		if err != nil {
			if depth > 0 {
				b.WriteString(original)
				i += open + 2 + close + 2
				continue
			}
			if errors.Is(err, ErrUnresolved) {
				return "", fmt.Errorf("{{%s}}: %w", body, err)
			}
			return "", fmt.Errorf("{{%s}}: %w", body, err)
		}
		if val == original {
			b.WriteString(val)
		} else {
			resolved, err := e.resolveDepth(val, depth+1)
			if err != nil {
				return "", err
			}
			b.WriteString(resolved)
		}
		i += open + 2 + close + 2
	}
	return b.String(), nil
}
