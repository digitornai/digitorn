package validate

import (
	"fmt"

	"github.com/mbathepaul/digitorn/internal/compiler/catalog"
	"github.com/mbathepaul/digitorn/internal/compiler/diagnostic"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/compiler/suggest"
)

// CheckHooks validates the params of every hook condition / action against
// the catalog of typed specs.
func CheckHooks(file string, def *schema.AppDefinition, cat *catalog.Catalog, bag *diagnostic.Bag) {
	for i, h := range def.RuntimeHooksOrNil() {
		checkHookParams(h, fmt.Sprintf("runtime.hooks.%d", i), cat, bag)
	}
	for i, a := range def.Agents {
		for j, h := range a.Hooks {
			checkHookParams(h, fmt.Sprintf("agents.%d.hooks.%d", i, j), cat, bag)
		}
	}
}

func checkHookParams(h schema.Hook, path string, cat *catalog.Catalog, bag *diagnostic.Bag) {
	if h.Condition.Type != "" {
		if spec, ok := cat.HookConditionSpec(string(h.Condition.Type)); ok {
			checkHookFields(h.Condition.Params, spec.Params,
				fmt.Sprintf("%s.condition", path), bag)
		}
	}
	if h.Action.Type != "" {
		if spec, ok := cat.HookActionSpec(string(h.Action.Type)); ok {
			checkHookFields(h.Action.Params, spec.Params,
				fmt.Sprintf("%s.action", path), bag)
		}
	}
}

func checkHookFields(params map[string]any, specs []catalog.HookParamSpec, path string, bag *diagnostic.Bag) {
	known := make(map[string]catalog.HookParamSpec, len(specs))
	for _, s := range specs {
		known[s.Name] = s
	}
	// Required check.
	for _, s := range specs {
		if !s.Required {
			continue
		}
		if _, ok := params[s.Name]; !ok {
			bag.Add(diagnostic.Errorf(diagnostic.CodeMissingRequired, posUnknown,
				"%s.%s: missing required field", path, s.Name))
		}
	}
	// Unknown + type checks.
	for name, value := range params {
		if name == "type" {
			continue
		}
		spec, ok := known[name]
		if !ok {
			pool := make([]string, 0, len(known))
			for k := range known {
				pool = append(pool, k)
			}
			d := diagnostic.Errorf(diagnostic.CodeUnknownField, posUnknown,
				"%s.%s: unknown field", path, name)
			if s, ok := suggest.Closest(name, pool, 2); ok {
				d = d.WithSuggestion(s, fmt.Sprintf("did you mean %q?", s))
			}
			bag.Add(d)
			continue
		}
		if err := matchHookType(spec.Type, value); err != nil {
			bag.Add(diagnostic.Errorf(diagnostic.CodeWrongType, posUnknown,
				"%s.%s: %s", path, name, err.Error()))
		}
		if len(spec.Enum) > 0 {
			s, _ := value.(string)
			if !stringIn(s, spec.Enum) {
				bag.Add(diagnostic.Errorf(diagnostic.CodeBadEnum, posUnknown,
					"%s.%s: %q not in %v", path, name, s, spec.Enum))
			}
		}
	}
}

func matchHookType(expected string, value any) error {
	switch expected {
	case "", "any":
		return nil
	case "string", "regex":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("expected string, got %T", value)
		}
	case "integer":
		switch value.(type) {
		case int, int32, int64, uint, uint32, uint64, float64:
		default:
			return fmt.Errorf("expected integer, got %T", value)
		}
	case "number":
		switch value.(type) {
		case int, int32, int64, uint, uint32, uint64, float32, float64:
		default:
			return fmt.Errorf("expected number, got %T", value)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("expected boolean, got %T", value)
		}
	case "string_list":
		if _, ok := value.([]any); !ok {
			return fmt.Errorf("expected array of strings, got %T", value)
		}
	case "string_or_string_list":
		// Convenience type used by params that accept either a single
		// match expression OR a list of them (e.g. tool_name.match).
		// Mirrors the lenient shape the Python daemon accepted.
		switch value.(type) {
		case string, []any:
		default:
			return fmt.Errorf("expected string or array of strings, got %T", value)
		}
	case "object":
		switch value.(type) {
		case map[string]any, []any:
		default:
			return fmt.Errorf("expected object or array, got %T", value)
		}
	}
	return nil
}

func stringIn(s string, list []string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
