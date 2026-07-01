package compiler

import (
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

func autoHook(def *schema.AppDefinition) *schema.Hook {
	if def.Runtime == nil {
		return nil
	}
	for i := range def.Runtime.Hooks {
		if def.Runtime.Hooks[i].ID == autoCompactHookID {
			return &def.Runtime.Hooks[i]
		}
	}
	return nil
}

// TestAutoCompact_DefaultOn : an app with no runtime.context at all still gets
// the synthetic hook (auto_compact defaults to true), with the doc-default
// threshold and the canonical condition/action/event.
func TestAutoCompact_DefaultOn(t *testing.T) {
	def := &schema.AppDefinition{}
	injectAutoCompact(def)

	h := autoHook(def)
	if h == nil {
		t.Fatal("auto_compact defaults on : expected an _auto_compact hook")
	}
	if h.On != schema.HookEventTurnStart {
		t.Errorf("on = %q, want turn_start", h.On)
	}
	if h.Condition.Type != schema.CondContextPressure {
		t.Errorf("condition = %q, want context_pressure", h.Condition.Type)
	}
	if got := h.Condition.Params["threshold"]; got != defaultCompressionTrigger {
		t.Errorf("threshold = %v, want %v", got, defaultCompressionTrigger)
	}
	if h.Action.Type != schema.ActionCompactContext {
		t.Errorf("action = %q, want compact_context", h.Action.Type)
	}
	if h.Cooldown != autoCompactCooldownSecs {
		t.Errorf("cooldown = %v, want %v", h.Cooldown, autoCompactCooldownSecs)
	}
}

// TestAutoCompact_ThresholdAndKnobsFromYAML : the threshold comes from
// compression_trigger and strategy/keep_recent are carried into the action —
// proving the value is YAML-driven, not hardcoded.
func TestAutoCompact_ThresholdAndKnobsFromYAML(t *testing.T) {
	def := &schema.AppDefinition{Runtime: &schema.RuntimeBlock{Context: &schema.ContextConfig{
		CompressionTrigger: 0.9,
		KeepRecent:         5,
		Strategy:           schema.ContextStrategy("summarize"),
	}}}
	injectAutoCompact(def)

	h := autoHook(def)
	if h == nil {
		t.Fatal("expected an _auto_compact hook")
	}
	if got := h.Condition.Params["threshold"]; got != 0.9 {
		t.Errorf("threshold = %v, want 0.9 (from compression_trigger)", got)
	}
	if got := h.Action.Params["keep_last"]; got != 5 {
		t.Errorf("keep_last = %v, want 5 (from keep_recent)", got)
	}
	if got := h.Action.Params["strategy"]; got != "summarize" {
		t.Errorf("strategy = %v, want summarize", got)
	}
}

// TestAutoCompact_DisabledByFlag : auto_compact:false suppresses the injection.
func TestAutoCompact_DisabledByFlag(t *testing.T) {
	off := false
	def := &schema.AppDefinition{Runtime: &schema.RuntimeBlock{Context: &schema.ContextConfig{AutoCompact: &off}}}
	injectAutoCompact(def)

	if autoHook(def) != nil {
		t.Fatal("auto_compact:false must not inject a hook")
	}
}

// TestAutoCompact_SkipsWhenExplicit : a hand-written compact_context hook fully
// replaces the default — no _auto_compact is added.
func TestAutoCompact_SkipsWhenExplicit(t *testing.T) {
	def := &schema.AppDefinition{Runtime: &schema.RuntimeBlock{Hooks: []schema.Hook{{
		ID:     "my_compactor",
		On:     schema.HookEventTurnStart,
		Action: schema.HookAction{Type: schema.ActionCompactContext},
	}}}}
	injectAutoCompact(def)

	if autoHook(def) != nil {
		t.Fatal("an explicit compact_context hook must suppress the auto-injection")
	}
	if len(def.Runtime.Hooks) != 1 {
		t.Fatalf("expected the single hand-written hook, got %d", len(def.Runtime.Hooks))
	}
}
