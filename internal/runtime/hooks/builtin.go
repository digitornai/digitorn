package hooks

import "github.com/digitornai/digitorn/internal/compiler/schema"

// BuiltinTaskCompletionGuardID is the id of the always-on stop hook that
// holds the turn open when the agent tries to finish with unfinished tasks.
const BuiltinTaskCompletionGuardID = "digitorn.builtin.task_completion_guard"

// taskCompletionReason is the directive injected when the guard fires. It is
// templated against the stop payload : {{tasks.summary}} lists the open tasks
// ("t2 (in_progress), t3 (pending)"). renderTemplate fills it at fire time.
const taskCompletionReason = "You ended your turn with open tasks: {{tasks.summary}}. Two cases — pick the right one:\n" +
	"1. If work still remains on a task, do NOT stop: take the next action that advances it, then call memory.task_update(task_id, status=\"completed\") (or status=\"blocked\" with a reason if it genuinely cannot proceed).\n" +
	"2. If you are deliberately WAITING ON THE USER — a checkpoint after a finished task, a clarifying question, their go-ahead before continuing — you MUST ask via the ask_user tool. ask_user PAUSES the turn for a real reply; a question typed as plain text does NOT pause, which is exactly why you landed back here. Re-ask the same thing through ask_user now.\n" +
	"Never stop silently with open tasks. End the turn only once every task is completed or blocked, OR you have called ask_user to wait for the user."


func BuiltinHooks() []schema.Hook {
	return []schema.Hook{
		{
			ID: BuiltinTaskCompletionGuardID,
			On: schema.HookEventStop,
			Condition: schema.HookCondition{
				Type:   schema.CondExpression,
				Params: map[string]any{"expr": "open_tasks > 0"},
			},
			Action: schema.HookAction{
				Type: schema.ActionGate,
				Params: map[string]any{
					"allow":  false,
					"reason": taskCompletionReason,
				},
			},
		},
	}
}


const BuiltinLSPDiagnoseHookID = "digitorn.builtin.lsp_diagnose"


func LSPDiagnoseHooks() []schema.Hook {
	return []schema.Hook{
		{
			ID: BuiltinLSPDiagnoseHookID,
			On: schema.HookEventToolEnd,
			Condition: schema.HookCondition{
				Type: schema.CondAllOf,
				Params: map[string]any{
					"conditions": []any{
						map[string]any{
							"type":  string(schema.CondToolName),
							"match": "filesystem.write|filesystem.edit|filesystem.multi_edit",
						},
						map[string]any{
							"type": string(schema.CondNot),
							"condition": map[string]any{
								"type": string(schema.CondToolFailed),
							},
						},
					},
				},
			},
			Action: schema.HookAction{
				Type: schema.ActionLSPDiagnose,
				Params: map[string]any{
					"path_field":    "tool.params.path",
					"content_field": "tool.params.content",
				},
			},
		},
	}
}
