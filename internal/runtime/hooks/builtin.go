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

// BuiltinHooks returns the runtime-default hooks every app gets for free,
// merged ahead of the app's declared runtime.hooks[]. They are ordinary
// schema.Hook values evaluated by the SAME engine — no special path — so the
// generic stop/inject machinery stays the single mechanism.
//
// Today there is one : a `stop` guard that vetoes the turn ending while the
// task plan has open work, injecting a reminder (the gate Reason) into the
// current turn. It only fires when open_tasks > 0, so apps without a task
// plan never see it. The engine caps how many times a turn may be held this
// way (anti-loop), so a genuinely stuck task can never wedge the loop.
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

// BuiltinLSPDiagnoseHookID is the id of the auto-diagnostics hook.
const BuiltinLSPDiagnoseHookID = "digitorn.builtin.lsp_diagnose"

// LSPDiagnoseHooks returns the auto-diagnostics hook the daemon adds for any app
// that grants the `lsp` module. After the agent writes/edits a file the hook
// syncs it to the language server and INJECTS the resulting errors/warnings into
// the agent's context — so it sees its mistakes immediately, without having to
// call a tool. Conditioned per-app (not in BuiltinHooks) so apps without lsp
// never pay for a no-op dispatch on every edit. Stays silent on a clean file.
//
// Only fires when the edit itself SUCCEEDED (tool_status == "completed") — a
// failed edit didn't change the file, so notifying the LSP would be a no-op
// and would confuse the agent by mixing the edit error with LSP diagnostics.
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
