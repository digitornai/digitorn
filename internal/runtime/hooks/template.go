package hooks

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Templating per docs-site/language/31-tool-hooks.md
// "Templating in actions". The four documented placeholders :
//
//	{{tool.name}}           → Payload.ToolName
//	{{tool.params.X}}       → Payload.ToolArgs[X] (dotted access)
//	{{tool.params.X.0.y}}   → array indexing supported
//	{{tool.result.X}}       → Payload.ToolResult[X]
//	{{tool.result}}         → whole result as JSON
//	{{tool.error}}          → Payload.ToolError
//
// The renderer auto-applies to the four templating actions
// (module_action, module_action_inject, pipe, shell) — applyTemplate
// walks any map[string]any / []any recursively and renders every
// string value.

var templatePattern = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_.\[\]]+)\s*\}\}`)

// renderTemplate returns s with every {{...}} occurrence replaced
// by the resolved value. Unknown paths render as the empty string
// — defensive : a misspelled placeholder doesn't crash the hook.
func renderTemplate(s string, p Payload) string {
	return templatePattern.ReplaceAllStringFunc(s, func(match string) string {
		sub := templatePattern.FindStringSubmatch(match)
		if len(sub) != 2 {
			return ""
		}
		val, ok := resolvePath(sub[1], p)
		if !ok {
			return ""
		}
		return stringify(val)
	})
}

// applyTemplate walks a generic params tree and renders every
// string leaf via renderTemplate. Maps and slices are visited
// in-place ; the function returns a new value of the same shape.
func applyTemplate(v any, p Payload) any {
	switch t := v.(type) {
	case string:
		return renderTemplate(t, p)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = applyTemplate(vv, p)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = applyTemplate(vv, p)
		}
		return out
	}
	return v
}

// resolvePath walks a dotted path against the Payload. Supports :
//
//	tool.name                      → ToolName
//	tool.error                     → ToolError
//	tool.result                    → ToolResult (rendered as JSON)
//	tool.result.X                  → ToolResult[X]
//	tool.result.X.Y                → ToolResult[X][Y]
//	tool.params.X.0.y              → ToolArgs[X][0][y]
//
// Returns (value, true) when the path resolves, (nil, false)
// otherwise.
func resolvePath(path string, p Payload) (any, bool) {
	parts := strings.Split(path, ".")
	if len(parts) == 0 {
		return nil, false
	}
	if parts[0] == "tasks" && len(parts) == 2 {
		switch parts[1] {
		case "summary":
			return p.TasksSummary, true
		case "open":
			return p.OpenTasks, true
		}
		return nil, false
	}
	if parts[0] != "tool" {
		return nil, false
	}
	if len(parts) == 1 {
		return p.ToolName, true
	}
	switch parts[1] {
	case "name":
		return p.ToolName, true
	case "error":
		return p.ToolError, true
	case "result":
		if len(parts) == 2 {
			b, _ := json.Marshal(p.ToolResult)
			return string(b), true
		}
		return walk(p.ToolResult, parts[2:])
	case "params":
		if len(parts) == 2 {
			b, _ := json.Marshal(p.ToolArgs)
			return string(b), true
		}
		return walk(p.ToolArgs, parts[2:])
	}
	return nil, false
}

// walk recursively descends `root` along the path segments. Each
// segment is either a map key (when root is map[string]any) or an
// integer index (when root is []any). Returns (nil, false) on
// any miss.
func walk(root any, segments []string) (any, bool) {
	cur := root
	for _, seg := range segments {
		if cur == nil {
			return nil, false
		}
		switch t := cur.(type) {
		case map[string]any:
			next, ok := t[seg]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(t) {
				return nil, false
			}
			cur = t[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

// stringify renders a resolved value to a string suitable for
// injection into a templated action param. Strings pass through ;
// scalars use fmt.Sprintf ; maps/slices serialise as JSON.
func stringify(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int, int32, int64, float32, float64:
		return fmt.Sprintf("%v", t)
	}
	b, err := json.Marshal(v)
	if err == nil {
		return string(b)
	}
	return fmt.Sprintf("%v", v)
}
