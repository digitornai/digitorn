package runtime

import (
	"strings"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/runtime/toolname"
)

// healToolArgs repairs the COMMON ways an LLM mis-shapes a tool call's arguments,
// so a confused model never dead-ends the agent on a "required param missing"
// error it has no way to see how to fix. This is the centralized, schema-driven
// cure for the whole class of "tools work randomly" failures whose real cause is
// the model keying a value under the wrong name (filesystem.glob called with
// {"glob": "**/*"} instead of {"pattern": "**/*"}).
//
// It is deliberately CONSERVATIVE — it only ever FILLS a required, still-empty
// string param. It never overwrites a value the model supplied, never invents
// data, and never relaxes validation for a genuinely-absent argument (a truly
// empty call still fails its tool's own validation, exactly as before).
//
// Two heals, most-confident first :
//
//  1. tool-name-as-param — the value is keyed under the tool's own short name
//     (glob/grep/...). The single most common confusion, and a whole family of
//     the "random" failures.
//  2. one-unambiguous-extra — exactly one required string param is still missing
//     AND exactly one supplied key is not in the schema → that lone stray key is
//     the misnamed param. Only fires when there is no ambiguity.
//
// Returns true when it changed args (for a debug log / telemetry). Mutates args
// in place, like the workdir path rewrite at the same dispatch chokepoint.
func healToolArgs(spec *tool.Spec, toolName string, args map[string]any) bool {
	if spec == nil || args == nil {
		return false
	}
	declared := make(map[string]bool, len(spec.Params))
	for _, p := range spec.Params {
		declared[p.Name] = true
	}
	healed := false

	// Heal 1 : value under the tool's own short name.
	short := shortToolName(toolName)
	for _, p := range spec.Params {
		if !healableMissing(p, args) || p.Name == short {
			continue
		}
		if v, ok := args[short]; ok && !isBlankArg(v) {
			args[p.Name] = v
			if !declared[short] {
				delete(args, short)
			}
			healed = true
		}
	}

	// Heal 2 : exactly one missing required string param + exactly one stray key.
	missing := requiredStringHoles(spec, args)
	if len(missing) == 1 {
		var stray []string
		for k, v := range args {
			if !declared[k] && !isBlankArg(v) {
				stray = append(stray, k)
			}
		}
		if len(stray) == 1 {
			if _, isStr := args[stray[0]].(string); isStr {
				args[missing[0]] = args[stray[0]]
				delete(args, stray[0])
				healed = true
			}
		}
	}
	return healed
}

// healableMissing reports whether p is a required string param with no usable
// value yet — the only kind heal 1 fills (non-string params are rarely mis-keyed
// and risky to coerce, so they're left to the tool's own validation).
func healableMissing(p tool.ParamSpec, args map[string]any) bool {
	return p.Required && (p.Type == "string" || p.Type == "") && isBlankArg(args[p.Name])
}

func requiredStringHoles(spec *tool.Spec, args map[string]any) []string {
	var out []string
	for _, p := range spec.Params {
		if healableMissing(p, args) {
			out = append(out, p.Name)
		}
	}
	return out
}

// isBlankArg treats a nil or whitespace-only string as "no usable value". A
// supplied non-string (number, bool, object) counts as present — never healed.
func isBlankArg(v any) bool {
	if v == nil {
		return true
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s) == ""
	}
	return false
}

// shortToolName is the action segment of a canonicalized tool name : the part an
// LLM tends to reuse as a parameter key. filesystem.glob → "glob".
func shortToolName(name string) string {
	n := toolname.Canonicalize(name)
	if i := strings.LastIndexByte(n, '.'); i >= 0 {
		return n[i+1:]
	}
	return n
}
