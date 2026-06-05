package hooks

import "github.com/mbathepaul/digitorn/internal/compiler/schema"

// BuiltinTaskCompletionGuardID is the id of the always-on stop hook that
// holds the turn open when the agent tries to finish with unfinished tasks.
const BuiltinTaskCompletionGuardID = "digitorn.builtin.task_completion_guard"

// taskCompletionReason is the directive injected when the guard fires. It is
// templated against the stop payload : {{tasks.summary}} lists the open tasks
// ("t2 (in_progress), t3 (pending)"). renderTemplate fills it at fire time.
const taskCompletionReason = "You are trying to end your turn with unfinished tasks: {{tasks.summary}}. Do NOT stop now. The work is not done while a task is pending or in_progress. Resume immediately: for each open task either do the remaining work and call memory.task_update(task_id, status=\"completed\"), or — only if it genuinely cannot proceed — call memory.task_update(task_id, status=\"blocked\") with the reason. Respond with the next action that advances a task, not a closing summary. You may only end the turn once every task is completed or blocked."

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
func LSPDiagnoseHooks() []schema.Hook {
	return []schema.Hook{
		{
			ID: BuiltinLSPDiagnoseHookID,
			On: schema.HookEventToolEnd,
			Condition: schema.HookCondition{
				Type:   schema.CondToolName,
				Params: map[string]any{"match": "filesystem.write|filesystem.edit|filesystem.multi_edit"},
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
