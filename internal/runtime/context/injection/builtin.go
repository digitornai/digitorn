package injection

import (
	"github.com/mbathepaul/digitorn/internal/llm"
)

// builtinToolSpecs holds the canonical schemas for the meta-tools +
// always-direct primitives the context_builder exposes regardless
// of injection mode.
//
// Documented in docs-site/docs/language/04b-builtin-tools.md
// "Always-available primitives (context_builder)" :
//
//	search_tools, get_tool, execute_tool, list_categories,
//	browse_category, run_parallel, use_skill, call_app, ask_user,
//	background_run
//
// CB-2 only defines the specs (data). CB-3 wires them to a real
// dispatcher so they execute. The descriptions below are intentionally
// terse + actionable so the LLM picks the right one without re-reading
// the doc — the doc-quote per-tool isn't repeated to keep input
// tokens low.
//
// Two notes :
//
//  1. background_run is documented as ONE action with 5 modes
//     dispatched by params (launch, status, wait, cancel, list_tasks).
//     The schema below carries the polymorphism in `parameters.oneOf`
//     so the LLM gets clear hints without 5 separate tools.
//
//  2. tool names are kept dotted ("context_builder.search_tools")
//     internally. The runtime adapter sanitizes them to underscore
//     form ("context_builder__search_tools") only when calling an
//     OpenAI-compatible API that rejects dots — that's a CB-3
//     responsibility.

// builtinToolSpecs holds the 5 context_builder primitives the planner draws
// from : the 3 discovery tools (search_tools — UNIFIED search + list + browse —
// plus get_tool, execute_tool) and the 2 universal execution primitives
// (run_parallel / background_run). builtinsForMode then picks the relevant
// subset per injection mode (no pollution).
//
// The other context_builder primitives are NOT universal and are appended by
// the wiring ONLY when usable (see CallAppSpec / AskUserSpec / UseSkillSpec) :
// injecting a tool the model can't actually use (no bridge wired, no skills,
// no grant) just invites small models to mis-pick it. agent_spawn.agent and
// memory.* are likewise module-gated.
//
// Order matters : in discovery mode the LLM sees them in this order.
func builtinToolSpecs() []llm.ToolSpec {
	return []llm.ToolSpec{
		// --- discovery meta-tools (5) ---
		{
			Name:        "context_builder.search_tools",
			Description: "Discover tools in the visible index — ONE tool, three modes by params: (1) query=\"read a file\" → hybrid semantic+keyword search, ranked hits ; (2) category=\"filesystem\" → list every tool in that domain (use page to paginate) ; (3) NO args → list the available domains/categories. After you find a tool, call get_tool for its exact parameters, then call it (or execute_tool).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Natural-language search, e.g. 'read a file', 'send HTTP POST'. Omit to browse/list instead.",
					},
					"category": map[string]any{
						"type":        "string",
						"description": "Domain/module to list in full, e.g. 'filesystem'. Used when query is empty.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Max search hits (query mode).",
						"default":     5,
					},
					"page": map[string]any{
						"type":        "integer",
						"description": "Page number when browsing a category.",
						"default":     1,
					},
				},
			},
		},
		{
			Name:        "context_builder.get_tool",
			Description: "Returns the full JSON schema (params, types, examples) for one tool. Call before execute_tool to know exactly which params to pass.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "Tool FQN, e.g. 'filesystem.read'.",
					},
				},
				"required": []string{"name"},
			},
		},
		{
			Name:        "context_builder.execute_tool",
			Description: "Execute any tool by name with the given parameters. The runtime also auto-routes direct calls (e.g. filesystem.read({...})) through this action.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "Tool FQN to call.",
					},
					"params": map[string]any{
						"type":        "object",
						"description": "Parameters object matching the tool's schema.",
					},
				},
				"required": []string{"name", "params"},
			},
		},
		// --- always-direct execution primitives (2) ---
		{
			Name:        "context_builder.run_parallel",
			Description: `Run several INDEPENDENT tool calls at once instead of one-by-one. Reach for this whenever you have 2+ calls whose inputs don't depend on each other — e.g. reading several files, grepping several patterns, hitting several endpoints. It's much faster than sequential calls and keeps the turn tight. Do NOT use it for steps that must run in order (a write that depends on a prior read), and don't wrap a single call in it. Pass "tasks" as a list of {tool, args}, e.g. {"tasks":[{"tool":"filesystem.read","args":{"path":"a.go"}},{"tool":"filesystem.read","args":{"path":"b.go"}}]}. Each task is isolated (one failing doesn't cancel the others) and results return in input order. 1-50 tasks.`,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tasks": map[string]any{
						"type":        "array",
						"description": "Tool calls to run concurrently (1-50). Each item is {tool, args}.",
						"minItems":    1,
						"maxItems":    50,
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"tool": map[string]any{"type": "string", "description": "Tool name, e.g. filesystem.read."},
								"args": map[string]any{"type": "object", "description": "Arguments for that tool."},
							},
							"required": []string{"tool", "args"},
						},
					},
				},
				"required": []string{"tasks"},
			},
		},
		{
			Name:        "context_builder.background_run",
			Description: "Run a SLOW or LONG-RUNNING tool off the critical path so the turn isn't blocked — use it for builds, installs, full test suites, downloads, migrations, or any shell command that may take more than a few seconds. Launch it, keep doing other useful work, then check `status` or `wait` later (and tell the user you started it). Don't sit blocked on something slow when you could be making progress elsewhere. One action, five modes by params: (1) launch: name+params → returns task_id ; (2) status: task_id ; (3) wait: task_id+wait=true+optional timeout ; (4) cancel: task_id+cancel=true ; (5) list: list_tasks=true. (To delegate work to a specialist, use the `agent` tool instead, not background_run.)",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":       map[string]any{"type": "string", "description": "Tool FQN to launch (mode 1)."},
					"params":     map[string]any{"type": "object", "description": "Parameters for the launched tool (mode 1)."},
					"task_id":    map[string]any{"type": "string", "description": "Existing task id (modes 2-4)."},
					"wait":       map[string]any{"type": "boolean", "description": "When true with task_id, block until completion (mode 3)."},
					"timeout":    map[string]any{"type": "number", "description": "Wait timeout in seconds (mode 3)."},
					"cancel":     map[string]any{"type": "boolean", "description": "When true with task_id, cancel the task (mode 4)."},
					"list_tasks": map[string]any{"type": "boolean", "description": "When true, list every background task of this session (mode 5)."},
				},
			},
		},
	}
}

// builtinsForMode returns the context_builder builtins RELEVANT for the chosen
// injection mode — the activation-by-relevance policy that stops polluting the
// agent's context with tools it can't use :
//
//   - hasTools == false (a pure-chat agent, no domain tools) → NONE. There is
//     nothing to discover, run in parallel or background.
//   - direct  : run_parallel + background_run only. Every domain tool is already
//     shown with its full schema, so the discovery / schema-fetch meta-tools are
//     dead weight.
//   - compact : get_tool + execute_tool (fetch the schema then call by name —
//     mandatory in compact, where tools have no inline params) + run_parallel +
//     background_run.
//   - discovery : the full 5 discovery meta-tools (domain tools are hidden
//     behind them) + run_parallel + background_run.
//
// Names stay dotted ; assembleToolList sanitizes to the wire form.
func builtinsForMode(mode Mode, hasTools bool) []llm.ToolSpec {
	if !hasTools {
		return nil
	}
	byName := make(map[string]llm.ToolSpec, 7)
	for _, s := range builtinToolSpecs() {
		byName[s.Name] = s
	}
	pick := func(names ...string) []llm.ToolSpec {
		out := make([]llm.ToolSpec, 0, len(names))
		for _, n := range names {
			if s, ok := byName["context_builder."+n]; ok {
				out = append(out, s)
			}
		}
		return out
	}
	switch mode {
	case ModeCompactDirect:
		return pick("get_tool", "execute_tool", "run_parallel", "background_run")
	case ModeDiscovery:
		// search_tools is the unified discovery tool (search + list + browse).
		return pick("search_tools", "get_tool", "execute_tool", "run_parallel", "background_run")
	default: // ModeDirect (and any fallback)
		return pick("run_parallel", "background_run")
	}
}

// CallAppSpec is the context_builder.call_app primitive. It is NOT a universal
// builtin : the wiring appends it ONLY when the daemon actually wired an
// AppCaller bridge (composition available). Injecting a non-wired call_app just
// gives the model a tool that returns "not wired" — which small models pick by
// mistake when they mean to delegate. Gating it on a real bridge removes that
// confusion. Name pre-sanitized to the OpenAI wire form.
func CallAppSpec() []llm.ToolSpec {
	specs := []llm.ToolSpec{
		{
			Name:        "context_builder.call_app",
			Description: "Invoke another DEPLOYED Digitorn app as a sub-tool (composition). NOT for delegating to a sub-agent — use the `agent` tool for that. Returns the called app's final agent reply.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"app_id": map[string]any{"type": "string", "description": "Target deployed app id."},
					"prompt": map[string]any{"type": "string", "description": "Message to send as the user input."},
				},
				"required": []string{"app_id", "prompt"},
			},
		},
	}
	for i := range specs {
		specs[i].Name = sanitizeToolName(specs[i].Name)
	}
	return specs
}

// AskUserSpec is the context_builder.ask_user primitive. Per
// docs-site/docs/reference/modules/context_builder.md it is exposed only when
// granted (tools.capabilities.grant {module: context_builder, actions:
// [ask_user]}) AND the daemon wired an AskUser bridge. The wiring appends it
// only when both hold. Name pre-sanitized.
func AskUserSpec() []llm.ToolSpec {
	specs := []llm.ToolSpec{
		{
			Name: "context_builder.ask_user",
			Description: "Ask the user a question and WAIT for their reply. The agent pauses until the user answers. " +
				"Escalate from simple to rich as the decision demands:\n" +
				"• Simple — ask_user(question=\"Should I proceed with this plan?\")\n" +
				"• Choices (clickable buttons) — ask_user(question=\"Which framework?\", choices=[\"FastAPI\",\"Django\",\"Flask\"])\n" +
				"• Multi-select — ask_user(question=\"Which features?\", choices=[\"Auth\",\"DB\",\"Tests\"], allow_multiple=true)\n" +
				"• Review/edit content — ask_user(question=\"Review this plan.\", content=\"## Plan\\n1. ...\") — the user may edit it; the edited text comes back\n" +
				"• Structured form — ask_user(question=\"Configure the project\", form=[{\"type\":\"select\",\"name\":\"framework\",\"label\":\"Framework\",\"options\":[\"FastAPI\",\"Django\"]},{\"type\":\"text\",\"name\":\"app_name\",\"label\":\"Name\"}])\n" +
				"Guidance: don't ask trivial questions you can decide yourself; ask ONE question per call (split multiple); use choices for 2–6 clear options; use a form for several related inputs at once.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"question": map[string]any{"type": "string", "description": "The question or message to show the user. Be specific about what you need."},
					"content":  map[string]any{"type": "string", "description": "Optional markdown (plan/code/config) for the user to review and edit. The possibly-edited text is returned."},
					"choices": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Optional clickable choices shown as buttons / a dropdown. The selection is returned as the reply.",
					},
					"allow_multiple": map[string]any{"type": "boolean", "description": "With choices, let the user pick several (returned comma-separated)."},
					"form": map[string]any{
						"type":  "array",
						"items": map[string]any{"type": "object"},
						"description": "Optional structured form. Each field: {type, name, label, options?, placeholder?, default?, required?}. " +
							"Types: select, text, textarea, checkbox, toggle, number. Replies come back as a JSON object.",
					},
					"timeout": map[string]any{"type": "number", "description": "Max seconds to wait (default 300)."},
				},
				"required": []string{"question"},
			},
		},
	}
	for i := range specs {
		specs[i].Name = sanitizeToolName(specs[i].Name)
	}
	return specs
}

// UseSkillSpec is the context_builder.use_skill primitive. Useless without
// skills, so the wiring appends it only when the app declares skills
// (dev.skills or the agent's capabilities.skills) AND a SkillLoader is wired.
// Name pre-sanitized.
func UseSkillSpec() []llm.ToolSpec {
	specs := []llm.ToolSpec{
		{
			Name:        "context_builder.use_skill",
			Description: "Invoke a /command skill declared in dev.skills. Returns the skill markdown to follow as instructions.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string", "description": "Slash command id (e.g. /commit). The leading / is optional."},
				},
				"required": []string{"command"},
			},
		},
	}
	for i := range specs {
		specs[i].Name = sanitizeToolName(specs[i].Name)
	}
	return specs
}

// MemoryToolSpecs are the 4 working-memory tools of the `memory` module
// (docs-site/docs/reference/modules/memory.md). UNLIKE the universal
// context_builder builtins above, these are NOT always injected : the wiring
// appends them ONLY when the app DECLARES the memory module in YAML
// (tools.modules.memory) — the documented opt-in contract. They keep their
// canonical `memory.*` FQN (gated like a module) but stay always-direct (the
// agent never has to discover how to manage its own memory). Names are
// pre-sanitized to the OpenAI wire form so the caller can append them as-is.
func MemoryToolSpecs() []llm.ToolSpec {
	specs := []llm.ToolSpec{
		{
			Name:        "memory.set_goal",
			Description: "Set the session's main objective. It stays visible in your working memory every turn and survives context compaction and resume. Use at the start of any non-trivial task.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"goal": map[string]any{
						"type":        "string",
						"description": "The objective, e.g. 'Fix the JWT verification bug in auth/client.go'.",
					},
				},
				"required": []string{"goal"},
			},
		},
		{
			Name:        "memory.remember",
			Description: "Store a durable fact (key finding, command, file path) that survives context compaction. Duplicates are auto-skipped; secrets are auto-redacted. Store results after completing work, not plans.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": map[string]any{
						"type":        "string",
						"description": "The fact, 1-2 sentences. e.g. 'Test command: go test ./auth/ -run TestVerify'.",
					},
				},
				"required": []string{"content"},
			},
		},
		{
			Name:        "memory.task_create",
			Description: "Add ONE step to your plan (visible to the user + read by the resume protocol). Call ≥2 times UP FRONT, before acting, whenever a request needs more than two steps or spans multiple files/phases. Each task is one specific, verifiable step — not a vague goal. Skip entirely for a single trivial step or a plain question; a one-item list is noise.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"subject": map[string]any{
						"type":        "string",
						"description": "Imperative one-liner, e.g. 'Patch validate.go null check'.",
					},
					"description": map[string]any{
						"type":        "string",
						"description": "Optional context for a cold-start resume (paths, params, why).",
					},
				},
				"required": []string{"subject"},
			},
		},
		{
			Name:        "memory.task_update",
			Description: "Update a step's status in REAL TIME. Keep EXACTLY ONE task 'in_progress' (set it the moment you start that step); flip it to 'completed' the instant it's truly done (never batch, never pre-mark); 'blocked' with a reason if stuck. Do NOT end your turn while any task is still pending or in_progress — finish the plan first. The runtime reads in_progress tasks to resume an interrupted turn.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id": map[string]any{
						"type":        "string",
						"description": "The task id, e.g. 't1'.",
					},
					"status": map[string]any{
						"type":        "string",
						"description": "One of: pending, in_progress, completed, blocked.",
					},
				},
				"required": []string{"task_id", "status"},
			},
		},
	}
	for i := range specs {
		specs[i].Name = sanitizeToolName(specs[i].Name)
	}
	return specs
}

// AgentToolSpec is the single delegation tool of the `agent_spawn` module
// (docs-site/docs/reference/modules/agent_spawn.md — one action, eight modes,
// tool name `Agent`). UNLIKE the universal context_builder builtins, it is NOT
// always injected : the wiring appends it ONLY when the app LOADS the
// agent_spawn module (declared in tools.modules.agent_spawn or granted via
// tools.capabilities.grant {module: agent_spawn}) — the documented gating. It
// keeps its canonical `agent_spawn.agent` FQN but stays always-direct. A second
// runtime gate (coordinator role) still applies at dispatch time. Name is
// pre-sanitized to the OpenAI wire form so the caller can append it as-is.
func AgentToolSpec() []llm.ToolSpec {
	specs := []llm.ToolSpec{
		{
			Name:        "agent_spawn.agent",
			Description: "Delegate to a specialist sub-agent (coordinator role only). Delegation is NON-BLOCKING by default: agent=<id>+task spawns the sub-agent and returns a run_id immediately, so you keep working while it runs. Do your own focused work — or fan out several delegations — then collect the result(s) later by calling THIS tool again with run_id (or run_ids=[...])+wait=true. To overlap, emit the delegation and your own tool calls in the SAME step; they dispatch concurrently. Only pass wait=true on the spawn itself when you have nothing else to do until the answer. Modes by params: (1) spawn (non-blocking): agent=<id>+task (+memory_seed) → run_id ; (2) status: run_id ; (3) collect: run_id (or run_ids=[...])+wait=true — run_ids are agent runs, NOT background tasks, so NEVER use background_run to wait on them ; (4) list: list=true ; (5) cancel: run_id+cancel=true. Each sub-agent runs in full isolation and returns a structured result.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"agent":       map[string]any{"type": "string", "description": "Target specialist agent id to spawn (mode 1)."},
					"task":        map[string]any{"type": "string", "description": "The instruction for the sub-agent (mode 1)."},
					"memory_seed": map[string]any{"type": "string", "description": "Read-only context (goal/facts) to brief the sub-agent (mode 1)."},
					"run_id":      map[string]any{"type": "string", "description": "Existing sub-agent run id (modes 2,3,5)."},
					"run_ids":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Several run ids to wait on (mode 3)."},
					"wait":        map[string]any{"type": "boolean", "description": "Block until the sub-agent(s) finish. Default false — spawning is non-blocking; prefer to spawn, keep working, then collect later with run_id/run_ids+wait=true (modes 1,3)."},
					"timeout":     map[string]any{"type": "number", "description": "Wait timeout in seconds (mode 3)."},
					"list":        map[string]any{"type": "boolean", "description": "List every sub-agent of this session (mode 4)."},
					"cancel":      map[string]any{"type": "boolean", "description": "Cancel the sub-agent run_id and its subtree (mode 5)."},
				},
			},
		},
	}
	for i := range specs {
		specs[i].Name = sanitizeToolName(specs[i].Name)
	}
	return specs
}
