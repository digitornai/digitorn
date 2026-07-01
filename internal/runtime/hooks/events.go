package hooks

import "github.com/digitornai/digitorn/internal/compiler/schema"

// canonicalEvent resolves the 3 documented aliases to their
// canonical event names per docs-site/language/31-tool-hooks.md
// "The events". A hook declared with on=pre_tool_use fires for
// the same event as on=tool_start.
//
//	pre_tool_use   → tool_start
//	post_tool_use  → tool_end
//	user_prompt    → turn_start
//	pre_finish     → stop
func canonicalEvent(e schema.HookEvent) schema.HookEvent {
	switch e {
	case schema.HookEventPreToolUse:
		return schema.HookEventToolStart
	case schema.HookEventPostToolUse:
		return schema.HookEventToolEnd
	case schema.HookEventUserPrompt:
		return schema.HookEventTurnStart
	case schema.HookEventPreFinish:
		return schema.HookEventStop
	}
	return e
}

// hookMatchesEvent reports whether a hook is registered for the
// given canonical event. Handles the schema field aliases (`on`,
// `event`, and the YAML 1.1 boolean-typo `true:` field).
func hookMatchesEvent(h schema.Hook, fired schema.HookEvent) bool {
	canonicalFired := canonicalEvent(fired)
	for _, raw := range []schema.HookEvent{h.On, h.Event, h.OnTrue} {
		if raw == "" {
			continue
		}
		if canonicalEvent(raw) == canonicalFired {
			return true
		}
	}
	return false
}
