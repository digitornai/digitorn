package compiler

import "github.com/mbathepaul/digitorn/internal/compiler/schema"

// Doc defaults for runtime.context (docs-site language/06-context-management).
const (
	defaultCompressionTrigger = 0.75
	autoCompactHookID         = "_auto_compact"
	autoCompactCooldownSecs   = 30.0
)

// injectAutoCompact materialises `runtime.context.auto_compact`. When it is on
// (the default) and the app declares no explicit compact_context hook, a
// synthetic _auto_compact hook is appended to runtime.hooks, driven by the
// context config: compression_trigger → the context_pressure threshold,
// strategy / keep_recent → the compact_context action.
//
// This is what makes the compaction threshold come from the YAML instead of a
// hardcoded fallback in the condition evaluator. The hook is built BEFORE
// validation, so CheckHooks vets it like any hand-written hook. An explicit
// compact_context hook (app- or agent-level) fully replaces the default, so an
// author keeps complete control.
func injectAutoCompact(def *schema.AppDefinition) {
	if def == nil {
		return
	}
	var cfg *schema.ContextConfig
	if def.Runtime != nil {
		cfg = def.Runtime.Context
	}
	// auto_compact defaults to true: nil config, or nil flag, means "on".
	if cfg != nil && cfg.AutoCompact != nil && !*cfg.AutoCompact {
		return
	}
	if hasExplicitCompactHook(def) {
		return
	}
	if def.Runtime == nil {
		def.Runtime = &schema.RuntimeBlock{}
	}
	def.Runtime.Hooks = append(def.Runtime.Hooks, buildAutoCompactHook(cfg))
}

// hasExplicitCompactHook reports whether the app already declares a
// compact_context action anywhere (runtime- or agent-scoped). When it does, the
// auto-injection is skipped so the hand-written hook is the sole compaction
// driver.
func hasExplicitCompactHook(def *schema.AppDefinition) bool {
	if def.Runtime != nil {
		for i := range def.Runtime.Hooks {
			if def.Runtime.Hooks[i].Action.Type == schema.ActionCompactContext {
				return true
			}
		}
	}
	for i := range def.Agents {
		for j := range def.Agents[i].Hooks {
			if def.Agents[i].Hooks[j].Action.Type == schema.ActionCompactContext {
				return true
			}
		}
	}
	return false
}

// buildAutoCompactHook assembles the synthetic hook. Only the threshold is
// always set (the condition needs it); strategy and keep_last are emitted only
// when the config declares them, so the runtime resolves the rest from the
// per-session context config and the documented defaults — no value is imposed
// here beyond the trigger.
func buildAutoCompactHook(cfg *schema.ContextConfig) schema.Hook {
	trigger := defaultCompressionTrigger
	if cfg != nil && cfg.CompressionTrigger > 0 {
		trigger = cfg.CompressionTrigger
	}

	action := map[string]any{}
	if cfg != nil && cfg.Strategy != "" {
		action["strategy"] = string(cfg.Strategy)
	}
	if cfg != nil && cfg.KeepRecent > 0 {
		action["keep_last"] = cfg.KeepRecent
	}

	return schema.Hook{
		ID: autoCompactHookID,
		On: schema.HookEventTurnStart,
		Condition: schema.HookCondition{
			Type:   schema.CondContextPressure,
			Params: map[string]any{"threshold": trigger},
		},
		Action: schema.HookAction{
			Type:   schema.ActionCompactContext,
			Params: action,
		},
		Cooldown: autoCompactCooldownSecs,
	}
}
