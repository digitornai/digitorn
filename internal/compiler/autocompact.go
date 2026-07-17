package compiler

import "github.com/digitornai/digitorn/internal/compiler/schema"

const (
	defaultCompressionTrigger = 0.97
	autoCompactHookID         = "_auto_compact"
	autoCompactCooldownSecs   = 30.0
)

func injectAutoCompact(def *schema.AppDefinition) {
	if def == nil {
		return
	}
	var cfg *schema.ContextConfig
	if def.Runtime != nil {
		cfg = def.Runtime.Context
	}
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
