package hooks

import "github.com/digitornai/digitorn/internal/compiler/schema"

const BuiltinTaskCompletionGuardID = "digitorn.builtin.task_completion_guard"

const taskCompletionReason = "Reminder: you still have open tasks: {{tasks.summary}}. Choose the case that fits — you decide:\n" +
	"1. You stopped by accident with work remaining: take the next action that advances a task, then memory.task_update(task_id, status=\"completed\") (or \"blocked\" with a reason).\n" +
	"2. You are waiting on the user (a checkpoint, a clarifying question, their go-ahead): use the ask_user tool. ask_user PAUSES the turn for a real reply; a question typed as plain text does NOT pause, which is why you were called back here — re-ask through ask_user.\n" +
	"3. The user deliberately told you to hold, pause, or just discuss: this is a legitimate stop. You ALREADY answered them above, so do NOT repeat yourself — reply with an EMPTY message (no text at all). An empty reply cleanly ends the turn; the open tasks stay pending for later, which is correct here. Do NOT resume the tasks and do NOT re-ask via ask_user.\n" +
	"Default to finishing or pausing via ask_user when unsure, but if the user asked you to stop, honour it: answer once, then end with an empty reply."


// TaskCompletionGuard is the built-in stop guard that vetoes a turn ending
// with open tasks. Kept defined but DISABLED: its reactive veto re-runs the
// turn (duplicate responses) and fires blindly on open_tasks>0, nagging even
// on legitimate stops (user redirect, waiting on input). Re-enable by adding
// it back to BuiltinHooks()'s slice.
var TaskCompletionGuard = schema.Hook{
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
}

func BuiltinHooks() []schema.Hook {
	// task_completion_guard is intentionally omitted (disabled). Add
	// TaskCompletionGuard here to re-enable.
	return []schema.Hook{}
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
