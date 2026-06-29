package schema

type HookEvent string

const (
	HookEventTurnStart       HookEvent = "turn_start"
	HookEventTurnEnd         HookEvent = "turn_end"
	HookEventStop            HookEvent = "stop"
	HookEventToolStart       HookEvent = "tool_start"
	HookEventToolEnd         HookEvent = "tool_end"
	HookEventPreToolUse      HookEvent = "pre_tool_use"
	HookEventPostToolUse     HookEvent = "post_tool_use"
	HookEventPreFinish       HookEvent = "pre_finish" // alias for stop
	HookEventUserPrompt      HookEvent = "user_prompt"
	HookEventSessionStart    HookEvent = "session_start"
	HookEventSessionEnd      HookEvent = "session_end"
	HookEventPreCompact      HookEvent = "pre_compact"
	HookEventError           HookEvent = "error"
	HookEventApprovalRequest HookEvent = "approval_request"
	HookEventAgentSpawn      HookEvent = "agent_spawn"
	HookEventAgentComplete   HookEvent = "agent_complete"
	HookEventActivation      HookEvent = "activation"
)

var AllHookEvents = []HookEvent{
	HookEventTurnStart, HookEventTurnEnd, HookEventStop,
	HookEventToolStart, HookEventToolEnd,
	HookEventPreToolUse, HookEventPostToolUse, HookEventPreFinish,
	HookEventUserPrompt,
	HookEventSessionStart, HookEventSessionEnd,
	HookEventPreCompact, HookEventError,
	HookEventApprovalRequest,
	HookEventAgentSpawn, HookEventAgentComplete,
	HookEventActivation,
}

// NotYetRoutedHookEvents lists documented events the CURRENT runtime
// build never emits, mapped to the reason. A hook declared on one of
// these compiles cleanly (the event is valid) but will not fire until
// the backing feature lands. The compiler emits a WARNING (never an
// error) so an author is not surprised by a silently-dead hook — this
// is the single source the lint reads.
//
// turn_start/turn_end/tool_start/tool_end (+ their aliases),
// session_start, session_end, pre_compact, error and approval_request
// ARE routed by the runtime and so are absent here.
var NotYetRoutedHookEvents = map[HookEvent]string{
	HookEventAgentSpawn:    "multi-agent is not implemented in this build",
	HookEventAgentComplete: "multi-agent is not implemented in this build",
	HookEventActivation:    "activation is declared-only and not routed at the hook layer",
}

type HookConditionType string

// The 14 built-in conditions, verbatim from
// docs-site/language/31-tool-hooks.md "Conditions (14 built-in)".
// This is the single source of truth : the compiler validates against
// it, the runtime dispatches against it, and a conformance test
// (internal/runtime/hooks/conformance_test.go) asserts doc == compiler
// == runtime so the three can never drift apart again.
const (
	CondAlways          HookConditionType = "always"
	CondNever           HookConditionType = "never"
	CondContextPressure HookConditionType = "context_pressure"
	CondTurnCount       HookConditionType = "turn_count"
	CondToolCalls       HookConditionType = "tool_calls"
	CondMessageCount    HookConditionType = "message_count"
	CondToolName        HookConditionType = "tool_name"
	CondToolFailed      HookConditionType = "tool_failed"
	CondContentContains HookConditionType = "content_contains"
	CondErrorType       HookConditionType = "error_type"
	CondExpression      HookConditionType = "expression"
	CondAllOf           HookConditionType = "all_of"
	CondAnyOf           HookConditionType = "any_of"
	CondNot             HookConditionType = "not"
)

var AllHookConditions = []HookConditionType{
	CondAlways, CondNever,
	CondContextPressure, CondTurnCount, CondToolCalls, CondMessageCount,
	CondToolName, CondToolFailed,
	CondContentContains, CondErrorType, CondExpression,
	CondAllOf, CondAnyOf, CondNot,
}

type HookActionType string

// The 15 built-in actions, verbatim from
// docs-site/language/31-tool-hooks.md "Actions (15 built-in)". The
// first 13 are general-purpose ; compile_yaml + auto_test_deploy are
// builder-app scoped (not intended for end-user YAMLs but accepted so
// the builder app compiles).
const (
	ActionCompactContext     HookActionType = "compact_context"
	ActionInjectMessage      HookActionType = "inject_message"
	ActionModuleAction       HookActionType = "module_action"
	ActionModuleActionInject HookActionType = "module_action_inject"
	ActionLog                HookActionType = "log"
	ActionShell              HookActionType = "shell"
	ActionGate               HookActionType = "gate"
	ActionTransformParams    HookActionType = "transform_params"
	ActionTransformResult    HookActionType = "transform_result"
	ActionChain              HookActionType = "chain"
	ActionNotify             HookActionType = "notify"
	ActionPipe               HookActionType = "pipe"
	ActionLSPDiagnose        HookActionType = "lsp_diagnose"
	ActionCompileYAML        HookActionType = "compile_yaml"
	ActionAutoTestDeploy     HookActionType = "auto_test_deploy"

	// ActionNoop is the internal empty action. NOT a documented
	// built-in : excluded from AllHookActions so a YAML can't declare
	// it, but the runtime treats it (and the empty string) as a no-op
	// for defensive dispatch.
	ActionNoop HookActionType = "noop"
)

var AllHookActions = []HookActionType{
	ActionCompactContext, ActionInjectMessage,
	ActionModuleAction, ActionModuleActionInject,
	ActionLog, ActionShell, ActionGate,
	ActionTransformParams, ActionTransformResult,
	ActionChain, ActionNotify, ActionPipe, ActionLSPDiagnose,
	ActionCompileYAML, ActionAutoTestDeploy,
}

type Hook struct {
	ID    string    `yaml:"id" json:"id"`
	On    HookEvent `yaml:"on,omitempty" json:"on,omitempty"`
	Event HookEvent `yaml:"event,omitempty" json:"event,omitempty"` // alias for `on`
	// OnTrue catches the case where editors save `on:` as bare YAML 1.1 and
	// it round-trips back as the boolean key `true:`.
	OnTrue    HookEvent     `yaml:"true,omitempty" json:"true,omitempty"`
	Condition HookCondition `yaml:"condition" json:"condition"`
	Action    HookAction    `yaml:"action" json:"action"`
	Cooldown  float64       `yaml:"cooldown,omitempty" json:"cooldown,omitempty"`
	MaxFires  int           `yaml:"max_fires,omitempty" json:"max_fires,omitempty"`
	Priority  int           `yaml:"priority,omitempty" json:"priority,omitempty"`
	Enabled   *bool         `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Timeout   float64       `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Tags      []string      `yaml:"tags,omitempty" json:"tags,omitempty"`
}

type HookCondition struct {
	Type   HookConditionType `yaml:"type" json:"type"`
	Params map[string]any    `yaml:",inline" json:"-"`
}

type HookAction struct {
	Type   HookActionType `yaml:"type" json:"type"`
	Params map[string]any `yaml:",inline" json:"-"`
}
