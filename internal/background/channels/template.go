package channels

import (
	"regexp"
	"strconv"
	"strings"
)

// maxRenderBytes caps a rendered string at 256 KB (Python template.py:262144).
const maxRenderBytes = 262144

// tmplRe matches {{ expr }} non-greedily, whitespace-tolerant (Python
// template.py: \{\{\s*(.+?)\s*\}\}).
var tmplRe = regexp.MustCompile(`\{\{\s*(.+?)\s*\}\}`)

// Render substitutes {{ dotpath }} expressions from scope in a SINGLE pass (the
// replacement text is never re-scanned, which blocks template-injection). It is
// pure substitution — no eval. `secret.*` and `env.*` are compile-time-only and
// are BLOCKED at runtime (rendered empty). Unresolved paths render empty. Output
// is truncated to maxRenderBytes.
func Render(tmpl string, scope map[string]any) string {
	if tmpl == "" || !strings.Contains(tmpl, "{{") {
		return clip(tmpl)
	}
	out := tmplRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		expr := strings.TrimSpace(tmplRe.FindStringSubmatch(m)[1])
		if isRuntimeBlocked(expr) {
			return ""
		}
		v, ok := resolveDotpath(scope, expr)
		if !ok {
			return ""
		}
		return stringify(v)
	})
	return clip(out)
}

// isRuntimeBlocked is true for compile-time-only scopes that must never resolve
// at runtime (security.py:16 / template.py:_replace).
func isRuntimeBlocked(expr string) bool {
	return strings.HasPrefix(expr, "secret.") || expr == "secret" ||
		strings.HasPrefix(expr, "env.") || expr == "env"
}

func clip(s string) string {
	if len(s) > maxRenderBytes {
		return s[:maxRenderBytes]
	}
	return s
}

// resolveDotpath walks a dotted path over nested maps and slices. List indices
// are integer segments (e.g. items.0.title). Returns (value, found).
func resolveDotpath(root any, path string) (any, bool) {
	cur := root
	for _, seg := range strings.Split(path, ".") {
		if seg == "" {
			return nil, false
		}
		switch c := cur.(type) {
		case map[string]any:
			v, ok := c[seg]
			if !ok {
				return nil, false
			}
			cur = v
		case map[any]any: // yaml.v3 can yield this for nested maps
			v, ok := c[seg]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(c) {
				return nil, false
			}
			cur = c[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// stringify renders a scalar the way Python str() would for the common JSON
// types: integral floats as integers ("5" not "5.0"), bools as true/false,
// nil as empty.
func stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	default:
		return ""
	}
}
