package catalog

import (
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/pkg/module"
)

// systemModuleManifests are the runtime-internal modules implemented as engine
// subsystems rather than service-bus modules: `memory` (MemoryWriter) and
// `agent_spawn` (AgentManager). They never appear in a ManifestSource built
// from the module registry, yet an app must be able to DECLARE them — that is
// the documented activation contract (docs-site/docs/language/04b-builtin-tools.md
// "Memory tools (gated by tools.modules.memory)" / "Agent-spawn tool (gated by
// agent_spawn module loaded)"). Seeding them into every Catalog lets
// `tools.modules.memory` / `tools.modules.agent_spawn` and capability grants on
// their actions validate at compile time, while the runtime keeps handling
// their dispatch in-process.
func systemModuleManifests() []module.Manifest {
	low := func(name, desc string, params ...tool.ParamSpec) tool.Spec {
		return tool.Spec{Name: name, Description: desc, RiskLevel: tool.RiskLow, Params: params}
	}
	str := func(name, desc string, required bool) tool.ParamSpec {
		return tool.ParamSpec{Name: name, Type: "string", Description: desc, Required: required}
	}
	return []module.Manifest{
		{
			ID:          "memory",
			Version:     "1.0.0",
			Description: "Cognitive working memory — goal, tasks, and durable facts re-injected every turn.",
			Tools: []tool.Spec{
				low("set_goal", "Set the session's high-level goal.",
					str("goal", "The objective.", true)),
				low("remember", "Store a durable fact that survives context compaction.",
					str("content", "The fact (1-2 sentences).", true)),
				low("task_create", "Add ONE step to your plan. Call it ≥2 times UP FRONT (before acting) for any request needing more than two steps or spanning multiple files; skip entirely for trivial one-step asks or plain questions.",
					str("subject", "A specific, verifiable step in the imperative ('add the /login route'), not a vague goal.", true),
					str("description", "Optional extra context for the step.", false)),
				low("task_update", "Update a step's status in REAL TIME : mark exactly one in_progress when you start it, completed the instant it's done, blocked (with reason) if stuck. Never finish your turn with a task still pending/in_progress.",
					str("task_id", "Task id (t1, t2, …) returned by task_create.", true),
					str("status", "pending | in_progress | completed | blocked.", true)),
			},
		},
		{
			ID:          "agent_spawn",
			Version:     "1.0.0",
			Description: "Dynamic sub-agent delegation — one action, several modes (spawn/wait/status/cancel/list).",
			Tools: []tool.Spec{
				low("agent", "Delegate to a specialist sub-agent (coordinator role only).",
					str("agent", "Target specialist agent id (spawn).", false),
					str("task", "Instruction for the sub-agent (spawn).", false)),
			},
		},
		{
			// context_builder hosts the meta-tools + human-in-the-loop
			// primitives. They are dispatched in-process (never on the
			// service bus), but an app must be able to grant / deny them —
			// notably `ask_user` and `call_app`, which are exposed ONLY
			// when granted (docs-site/docs/language/04-tools.md
			// "context_builder primitives"). Seeding the module lets those
			// capability grants validate at compile time.
			ID:          "context_builder",
			Version:     "1.0.0",
			Description: "Tool discovery + human-in-the-loop primitives (search_tools, get_tool, execute_tool, run_parallel, background_run, use_skill, call_app, ask_user).",
			Tools: []tool.Spec{
				low("search_tools", "Discover tools by query / category."),
				low("get_tool", "Fetch one tool's full schema."),
				low("execute_tool", "Execute a discovered tool by name."),
				low("run_parallel", "Run several tool calls concurrently."),
				low("background_run", "Launch / manage a background task."),
				low("use_skill", "Load a /command skill's instructions."),
				low("call_app", "Invoke another deployed app as a sub-tool."),
				low("ask_user", "Pause the turn and ask the user a question.",
					str("question", "The question to put to the user.", true)),
			},
		},
	}
}
