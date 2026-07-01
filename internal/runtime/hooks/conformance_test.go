package hooks

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/catalog"
	"github.com/digitornai/digitorn/internal/compiler/schema"
)

// =====================================================================
// CONFORMANCE LOCK — the anti-divergence verifier.
//
// This test is the single thing that keeps the hook vocabulary honest.
// It asserts that the doc, the compiler schema enum, the compiler param
// catalog, and the runtime dispatch all describe the EXACT SAME set of
// conditions and actions. If anyone adds an action to the catalog
// without a runtime handler, renames a condition, or drifts from the
// documentation, this test fails in CI — divergence becomes impossible.
//
// docD... lists are transcribed verbatim from
// docs-site/language/31-tool-hooks.md. They are the source of truth ;
// everything else must equal them.
// =====================================================================

// docConditions — "Conditions (14 built-in)".
var docConditions = []string{
	"always", "never",
	"context_pressure", "turn_count", "tool_calls", "message_count",
	"tool_name", "tool_failed",
	"content_contains", "error_type", "expression",
	"all_of", "any_of", "not",
}

// docActions — "Actions (15 built-in)" (13 general + 2 builder).
var docActions = []string{
	"compact_context", "inject_message",
	"module_action", "module_action_inject",
	"log", "shell", "gate",
	"transform_params", "transform_result",
	"chain", "notify", "pipe", "lsp_diagnose",
	"compile_yaml", "auto_test_deploy",
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func equalSets(t *testing.T, label string, got, want []string) {
	t.Helper()
	g, w := sortedCopy(got), sortedCopy(want)
	if len(g) != len(w) {
		t.Errorf("%s: %d entries, want %d\n got=%v\nwant=%v", label, len(g), len(w), g, w)
		return
	}
	for i := range g {
		if g[i] != w[i] {
			t.Errorf("%s: set mismatch at %d : got %q want %q\n got=%v\nwant=%v",
				label, i, g[i], w[i], g, w)
			return
		}
	}
}

// =====================================================================
// 1. Compiler schema enum == doc
// =====================================================================

func TestConformance_SchemaConditionsMatchDoc(t *testing.T) {
	got := make([]string, len(schema.AllHookConditions))
	for i, c := range schema.AllHookConditions {
		got[i] = string(c)
	}
	equalSets(t, "schema.AllHookConditions vs doc", got, docConditions)
}

func TestConformance_SchemaActionsMatchDoc(t *testing.T) {
	got := make([]string, len(schema.AllHookActions))
	for i, a := range schema.AllHookActions {
		got[i] = string(a)
	}
	equalSets(t, "schema.AllHookActions vs doc", got, docActions)
}

// =====================================================================
// 2. Compiler param catalog == doc
// =====================================================================

func TestConformance_CatalogConditionsMatchDoc(t *testing.T) {
	got := make([]string, 0, len(catalog.HookConditions))
	for _, s := range catalog.HookConditions {
		got = append(got, s.Name)
	}
	equalSets(t, "catalog.HookConditions vs doc", got, docConditions)
}

func TestConformance_CatalogActionsMatchDoc(t *testing.T) {
	got := make([]string, 0, len(catalog.HookActions))
	for _, s := range catalog.HookActions {
		got = append(got, s.Name)
	}
	equalSets(t, "catalog.HookActions vs doc", got, docActions)
}

// =====================================================================
// 3. Runtime implements every doc condition (none falls to default-false
//    silently). For each condition we craft a payload that MUST fire it.
// =====================================================================

func TestConformance_RuntimeHandlesEveryCondition(t *testing.T) {
	cases := []struct {
		typ     string
		params  map[string]any
		payload Payload
		want    bool
	}{
		{"always", nil, Payload{}, true},
		{"never", nil, Payload{}, false},
		{"context_pressure", map[string]any{"threshold": 0.5}, Payload{TokensUsed: 80, MaxTokens: 100}, true},
		{"turn_count", map[string]any{"threshold": 1}, Payload{TurnCount: 1}, true},
		{"tool_calls", map[string]any{"threshold": 1}, Payload{ToolCallsUsed: 2}, true},
		{"message_count", map[string]any{"threshold": 1}, Payload{MessageCount: 3}, true},
		{"tool_name", map[string]any{"match": "filesystem.read"}, Payload{ToolName: "filesystem.read"}, true},
		{"tool_failed", nil, Payload{ToolStatus: "errored"}, true},
		{"content_contains", map[string]any{"keyword": "secret"}, Payload{LLMContent: "the secret is out"}, true},
		{"error_type", map[string]any{"match": "boom"}, Payload{ErrorType: "boom: kaboom"}, true},
		{"expression", map[string]any{"expr": "tokens_used > 5"}, Payload{TokensUsed: 10}, true},
		{"all_of", map[string]any{"conditions": []any{map[string]any{"type": "always"}}}, Payload{}, true},
		{"any_of", map[string]any{"conditions": []any{map[string]any{"type": "always"}}}, Payload{}, true},
		{"not", map[string]any{"condition": map[string]any{"type": "never"}}, Payload{}, true},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		seen[c.typ] = true
		got := EvalCondition(schema.HookCondition{
			Type:   schema.HookConditionType(c.typ),
			Params: c.params,
		}, c.payload)
		if got != c.want {
			t.Errorf("condition %q : EvalCondition = %v, want %v (handler missing or wrong?)",
				c.typ, got, c.want)
		}
	}
	// Every doc condition must have a case here.
	for _, name := range docConditions {
		if !seen[name] {
			t.Errorf("doc condition %q has no runtime conformance case", name)
		}
	}
}

// =====================================================================
// 4. Runtime implements every doc action (RunAction never returns the
//    "unsupported action type" sentinel for a documented action).
// =====================================================================

func TestConformance_RuntimeHandlesEveryAction(t *testing.T) {
	// Minimal params so each action gets past its own arg-parsing to
	// prove the DISPATCH case exists. We don't wire deps : actions that
	// need a Caller/Sink return their own "not wired" error, which is
	// fine — the only forbidden outcome is "unsupported action type".
	params := map[string]map[string]any{
		"compact_context":      {},
		"inject_message":       {"content": "hi"},
		"module_action":        {"action": "noop"},
		"module_action_inject": {"action": "noop"},
		"log":                  {"message": "hi"},
		"shell":                {"command": "echo hi"},
		"gate":                 {"allow": true},
		"transform_params":     {"transformation": map[string]any{"x": 1}},
		"transform_result":     {"transformation": map[string]any{"x": 1}},
		"chain":                {"actions": []any{}},
		"notify":               {"title": "t", "message": "m"},
		"pipe":                 {"to": "filesystem.read"},
		"lsp_diagnose":         {"path_field": "tool.params.path"},
		"compile_yaml":         {"path": "app.yaml"},
		"auto_test_deploy":     {},
	}
	for _, name := range docActions {
		p, ok := params[name]
		if !ok {
			t.Errorf("doc action %q has no runtime conformance case", name)
			continue
		}
		_, err := RunAction(context.Background(), schema.HookAction{
			Type:   schema.HookActionType(name),
			Params: p,
		}, Payload{ToolName: "filesystem.read", ToolArgs: map[string]any{"path": "x"}}, ActionDeps{})
		if err != nil && strings.Contains(err.Error(), "unsupported action type") {
			t.Errorf("action %q : RunAction reports unsupported — dispatch case missing", name)
		}
	}
}
