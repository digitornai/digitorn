package validate

import (
	"fmt"

	"github.com/digitornai/digitorn/internal/compiler/catalog"
	"github.com/digitornai/digitorn/internal/compiler/diagnostic"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/compiler/suggest"
	"github.com/digitornai/digitorn/internal/domain/tool"
)

// CheckParams runs the parameter-validation pass: setup steps and hook
// module_action params are typechecked against the tool's ParamSpec list
// pulled from the catalog.
func CheckParams(file string, def *schema.AppDefinition, cat *catalog.Catalog, bag *diagnostic.Bag) {
	if def.Tools != nil {
		for modID, block := range def.Tools.Modules {
			for i, step := range block.Setup {
				path := fmt.Sprintf("tools.modules.%s.setup.%d", modID, i)
				validateSetupStep(modID, step, path, cat, bag)
			}
		}
	}
	for i, h := range def.RuntimeHooksOrNil() {
		validateHookAction(h, fmt.Sprintf("runtime.hooks.%d.action", i), cat, bag)
	}
	for i, a := range def.Agents {
		for j, h := range a.Hooks {
			validateHookAction(h, fmt.Sprintf("agents.%d.hooks.%d.action", i, j), cat, bag)
		}
	}
}

func validateSetupStep(modID string, step schema.SetupStep, path string, cat *catalog.Catalog, bag *diagnostic.Bag) {
	if !cat.HasModule(modID) {
		return
	}
	spec, ok := cat.ToolSpec(modID, step.Action)
	if !ok {
		if s, ok := suggest.Closest(step.Action, cat.ToolsFor(modID), 2); ok {
			bag.Add(diagnostic.Errorf(diagnostic.CodeUnknownTool, posUnknown,
				"%s.action: module %q has no tool %q", path, modID, step.Action).
				WithSuggestion(s, fmt.Sprintf("did you mean %q?", s)))
		} else {
			bag.Add(diagnostic.Errorf(diagnostic.CodeUnknownTool, posUnknown,
				"%s.action: module %q has no tool %q", path, modID, step.Action))
		}
		return
	}
	checkParamsAgainstSpec(step.Params, spec.Params, fmt.Sprintf("%s.params", path), bag)
}

func validateHookAction(h schema.Hook, path string, cat *catalog.Catalog, bag *diagnostic.Bag) {
	if h.Action.Type != schema.ActionModuleAction {
		return
	}
	modID, _ := h.Action.Params["module"].(string)
	action, _ := h.Action.Params["action"].(string)
	if modID == "" || action == "" {
		return
	}
	if !cat.HasModule(modID) {
		return
	}
	spec, ok := cat.ToolSpec(modID, action)
	if !ok {
		return
	}
	params, _ := h.Action.Params["params"].(map[string]any)
	checkParamsAgainstSpec(params, spec.Params, path+".params", bag)
}

// checkParamsAgainstSpec validates that every required param is provided, every
// provided param exists, and that every value's type matches the declared one.
func checkParamsAgainstSpec(params map[string]any, specs []tool.ParamSpec, path string, bag *diagnostic.Bag) {
	known := make(map[string]tool.ParamSpec, len(specs))
	for _, s := range specs {
		known[s.Name] = s
	}
	for _, s := range specs {
		if !s.Required {
			continue
		}
		if _, ok := params[s.Name]; !ok {
			bag.Add(diagnostic.Errorf(diagnostic.CodeMissingRequired, posUnknown,
				"%s.%s: missing required parameter", path, s.Name))
		}
	}
	for name, value := range params {
		spec, ok := known[name]
		if !ok {
			pool := make([]string, 0, len(known))
			for k := range known {
				pool = append(pool, k)
			}
			d := diagnostic.Errorf(diagnostic.CodeUnknownField, posUnknown,
				"%s.%s: unknown parameter", path, name)
			if s, ok := suggest.Closest(name, pool, 2); ok {
				d = d.WithSuggestion(s, fmt.Sprintf("did you mean %q?", s))
			}
			bag.Add(d)
			continue
		}
		if err := typeMatch(spec, value); err != nil {
			bag.Add(diagnostic.Errorf(diagnostic.CodeWrongType, posUnknown,
				"%s.%s: %s", path, name, err.Error()))
		}
		if len(spec.Enum) > 0 && !inEnum(value, spec.Enum) {
			bag.Add(diagnostic.Errorf(diagnostic.CodeBadEnum, posUnknown,
				"%s.%s: %v not in enum %v", path, name, value, spec.Enum))
		}
	}
}

func typeMatch(spec tool.ParamSpec, value any) error {
	switch spec.Type {
	case "", "any":
		return nil
	case "string":
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
	case "array":
		if _, ok := value.([]any); !ok {
			return fmt.Errorf("expected array, got %T", value)
		}
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("expected object, got %T", value)
		}
	}
	return nil
}

func inEnum(value any, allowed []any) bool {
	for _, a := range allowed {
		if a == value {
			return true
		}
	}
	return false
}
