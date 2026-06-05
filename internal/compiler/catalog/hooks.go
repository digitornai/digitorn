package catalog

// HookConditionSpec declares the param schema for one condition type. The
// validator (validate.CheckHookParams) checks every hook.condition map
// against this — unknown keys become DGT-E0101, missing required keys become
// DGT-E0100, type mismatches become DGT-E0102.
type HookConditionSpec struct {
	Name   string
	Params []HookParamSpec
}

type HookActionSpec struct {
	Name   string
	Params []HookParamSpec
}

type HookParamSpec struct {
	Name     string
	Type     string // string | string_list | string_or_string_list | integer | boolean | object | regex
	Required bool
	Enum     []string
}

// HookConditions enumerates the 14 built-in conditions, verbatim from
// docs-site/language/31-tool-hooks.md "Conditions (14 built-in)". This
// is the single source of truth for param validation ; names mirror
// schema.AllHookConditions (asserted equal by the conformance test).
var HookConditions = []HookConditionSpec{
	{Name: "always", Params: nil},
	{Name: "never", Params: nil},
	{Name: "context_pressure", Params: []HookParamSpec{
		{Name: "threshold", Type: "number", Required: true},
	}},
	{Name: "turn_count", Params: []HookParamSpec{
		{Name: "threshold", Type: "integer", Required: true},
		{Name: "every", Type: "integer"},
	}},
	{Name: "tool_calls", Params: []HookParamSpec{
		{Name: "threshold", Type: "integer", Required: true},
	}},
	{Name: "message_count", Params: []HookParamSpec{
		{Name: "threshold", Type: "integer", Required: true},
	}},
	{Name: "tool_name", Params: []HookParamSpec{
		// `match` accepts a single fnmatch pattern ("filesystem.write")
		// or a list. Pipe / wildcard delimiters resolve at runtime.
		{Name: "match", Type: "string_or_string_list", Required: true},
	}},
	{Name: "tool_failed", Params: nil},
	{Name: "content_contains", Params: []HookParamSpec{
		{Name: "keyword", Type: "string", Required: true},
	}},
	{Name: "error_type", Params: []HookParamSpec{
		{Name: "match", Type: "regex", Required: true},
	}},
	{Name: "expression", Params: []HookParamSpec{
		{Name: "expr", Type: "string", Required: true},
	}},
	{Name: "all_of", Params: []HookParamSpec{
		{Name: "conditions", Type: "object", Required: true},
	}},
	{Name: "any_of", Params: []HookParamSpec{
		{Name: "conditions", Type: "object", Required: true},
	}},
	{Name: "not", Params: []HookParamSpec{
		{Name: "condition", Type: "object", Required: true},
	}},
}

// HookActions enumerates the 15 built-in actions, verbatim from
// docs-site/language/31-tool-hooks.md "Actions (15 built-in)". The
// first 13 are general-purpose ; compile_yaml + auto_test_deploy are
// builder-app scoped. Names mirror schema.AllHookActions.
var HookActions = []HookActionSpec{
	{Name: "compact_context", Params: []HookParamSpec{
		{Name: "strategy", Type: "string", Enum: []string{"truncate", "summarize"}},
		{Name: "keep_last", Type: "integer"},
	}},
	{Name: "inject_message", Params: []HookParamSpec{
		{Name: "content", Type: "string", Required: true},
		{Name: "role", Type: "string"},
		{Name: "placeholder", Type: "string"},
	}},
	{Name: "module_action", Params: []HookParamSpec{
		{Name: "module", Type: "string"},
		{Name: "action", Type: "string", Required: true},
		{Name: "params", Type: "object"},
		{Name: "action_params", Type: "object"},
	}},
	{Name: "module_action_inject", Params: []HookParamSpec{
		{Name: "module", Type: "string"},
		{Name: "action", Type: "string", Required: true},
		{Name: "params", Type: "object"},
		{Name: "action_params", Type: "object"},
		{Name: "role", Type: "string"},
	}},
	{Name: "log", Params: []HookParamSpec{
		{Name: "message", Type: "string", Required: true},
		{Name: "level", Type: "string"},
	}},
	{Name: "shell", Params: []HookParamSpec{
		{Name: "command", Type: "string", Required: true},
		{Name: "cwd", Type: "string"},
		{Name: "timeout", Type: "integer"},
		{Name: "on_error", Type: "string", Enum: []string{"log", "ignore", "raise"}},
	}},
	{Name: "gate", Params: []HookParamSpec{
		{Name: "reason", Type: "string"},
		{Name: "allow", Type: "boolean"},
	}},
	{Name: "transform_params", Params: []HookParamSpec{
		{Name: "transformation", Type: "object", Required: true},
	}},
	{Name: "transform_result", Params: []HookParamSpec{
		{Name: "transformation", Type: "object", Required: true},
	}},
	{Name: "chain", Params: []HookParamSpec{
		{Name: "actions", Type: "object", Required: true},
	}},
	{Name: "notify", Params: []HookParamSpec{
		{Name: "title", Type: "string"},
		{Name: "message", Type: "string"},
		{Name: "level", Type: "string"},
		{Name: "tag", Type: "string"},
	}},
	{Name: "pipe", Params: []HookParamSpec{
		{Name: "to", Type: "string", Required: true},
		{Name: "map", Type: "object"},
		{Name: "extra", Type: "object"},
		{Name: "on_error", Type: "string", Enum: []string{"log", "ignore", "raise"}},
	}},
	{Name: "lsp_diagnose", Params: []HookParamSpec{
		{Name: "path_field", Type: "string"},
		{Name: "content_field", Type: "string"},
		{Name: "publish", Type: "boolean"},
		{Name: "inject_result", Type: "boolean"},
		{Name: "read_from_disk", Type: "boolean"},
	}},
	{Name: "compile_yaml", Params: []HookParamSpec{
		{Name: "path", Type: "string"},
	}},
	{Name: "auto_test_deploy", Params: nil},
}

// hookSpecsIndex builds an O(1) lookup keyed by name.
func hookSpecsIndex[T any](items []T, keyOf func(T) string) map[string]T {
	out := make(map[string]T, len(items))
	for _, it := range items {
		out[keyOf(it)] = it
	}
	return out
}

func (c *Catalog) HookConditionSpec(name string) (HookConditionSpec, bool) {
	s, ok := c.hookConditions[name]
	return s, ok
}

func (c *Catalog) HookActionSpec(name string) (HookActionSpec, bool) {
	s, ok := c.hookActions[name]
	return s, ok
}
