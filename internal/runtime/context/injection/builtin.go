package injection

import (
	"github.com/digitornai/digitorn/internal/llm"
)

func builtinToolSpecs() []llm.ToolSpec {
	return []llm.ToolSpec{
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
			Description: "Execute any tool by name with the given parameters. The runtime also auto-routes direct calls (e.g. filesystem.read({...})) through this action. Pass `params` as a native JSON OBJECT, never a stringified blob — a hand-escaped string breaks on large values. For a large or JSON-heavy file body, use the target tool's `content_b64` (base64) field so escaping can't fail.",
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
			Name: "context_builder.background_run",
			Description: "Run any tool or shell command off the critical path — never block a turn on something slow. " +
				"Launch it, keep doing useful work in the same turn, then get the result automatically or check it later.\n\n" +
				"MODES (select by params):\n" +
				"  LAUNCH   name+params → task_id. Start a tool in the background. Do other work immediately after.\n" +
				"  STATUS   task_id → current state + live log tail. Poll progress without waiting.\n" +
				"  WAIT     task_id+wait=true+timeout? → blocks until completion. Use only when you truly have nothing else to do.\n" +
				"  CANCEL   task_id+cancel=true → stops the task.\n" +
				"  LIST     list_tasks=true → all running/recent tasks this session.\n\n" +
				"SMART WAITING (preferred over polling):\n" +
				"  notify_when: \"pattern\" — launch+set: agent is AUTO-WOKEN in a NEW TURN the moment the pattern\n" +
				"    appears in the output. No polling, no blocking. You get [BACKGROUND TASK READY] with the relevant\n" +
				"    log tail. Use for: servers starting, tests passing, builds completing, any recognizable line.\n" +
				"    Example: background_run(name=\"bash.run\", params={command:\"npm run dev\"}, notify_when=\"ready on port 3000\")\n" +
				"  wait_for: \"pattern\"+task_id — sync poll: block THIS turn until pattern seen (timeout default 60s).\n" +
				"    Use for short waits (< 30s) when you need the result before continuing.\n\n" +
				"WATCH MODE (run command repeatedly):\n" +
				"  watch=true+command+interval(s)+until(optional pattern) → runs command every N seconds as a loop.\n" +
				"  If until is set, stops and notifies when the pattern appears. Ideal for: health checks, waiting\n" +
				"  for a container to become healthy, watching a log file for an event.\n" +
				"  Example: background_run(watch=true, command=\"docker ps\", interval=2, until=\"healthy\")\n\n" +
				"SIGNALS & STDIN (for running tasks):\n" +
				"  signal: \"SIGINT\"|\"SIGTERM\" + task_id → send graceful signal. SIGINT = Ctrl+C, SIGTERM = graceful stop.\n" +
				"    Use SIGINT to stop a server cleanly, SIGTERM for graceful shutdown, vs cancel=true (SIGKILL).\n" +
				"  stdin: \"text\\n\" + task_id → pipe text to task stdin. Answers interactive prompts, feeds REPLs.\n\n" +
				"OUTPUT CONTROL:\n" +
				"  tail_lines: N — how many recent log lines to return (default 100, 0=all 64KB window).\n" +
				"  settle_seconds: N — how long to wait for fast-failing tasks before returning task_id (default 2s).\n\n" +
				"GOLDEN RULES:\n" +
				"  • NEVER block on slow work — launch it and continue. Use notify_when to be woken automatically.\n" +
				"  • Prefer notify_when over wait or polling — the agent is woken the instant the pattern appears.\n" +
				"  • Fan-out: launch multiple tasks in ONE step, they run in parallel. Collect later.\n" +
				"  • For specialist sub-agents use the `agent` tool instead of background_run.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":   map[string]any{"type": "string", "description": "Tool FQN to run in background, e.g. \"bash.run\". Required for launch."},
					"params": map[string]any{"type": "object", "description": "Parameters for the tool being launched."},
					"notify_when": map[string]any{"type": "string", "description": "Pattern to watch for in live output. When found, the agent is automatically woken in a NEW TURN with [BACKGROUND TASK READY] — no polling needed. Set at launch time alongside name+params."},
					"wait_for": map[string]any{"type": "string", "description": "Block THIS turn until this pattern appears in the task's live log (polls every 300ms). Requires task_id. Use for short waits < 30s."},
					"watch":    map[string]any{"type": "boolean", "description": "Run command repeatedly every interval seconds as a background loop."},
					"command":  map[string]any{"type": "string", "description": "Shell command to run repeatedly in watch mode."},
					"interval": map[string]any{"type": "number", "description": "Watch interval in seconds (default 2)."},
					"until":    map[string]any{"type": "string", "description": "Stop watching when this pattern appears in the output."},
					"task_id": map[string]any{"type": "string", "description": "ID of an existing background task (for status/wait/cancel/signal/stdin/wait_for)."},
					"wait":       map[string]any{"type": "boolean", "description": "Block until task completes. Set with task_id. Use only when you have nothing else to do."},
					"timeout":    map[string]any{"type": "number", "description": "Timeout in seconds for wait or wait_for (default: wait=∞, wait_for=60)."},
					"tail_lines": map[string]any{"type": "number", "description": "Lines of live log to return in status/wait (default 100, 0=all)."},
					"signal": map[string]any{"type": "string", "description": "Signal to send to running task: \"SIGINT\" (Ctrl+C graceful), \"SIGTERM\" (graceful stop). Requires task_id."},
					"stdin": map[string]any{"type": "string", "description": "Text to pipe to task stdin (e.g. \"yes\\n\" to answer a prompt). Requires task_id."},
					"cancel":     map[string]any{"type": "boolean", "description": "Kill the task (SIGKILL). Requires task_id."},
					"list_tasks": map[string]any{"type": "boolean", "description": "List all background tasks for this session."},
					"settle_seconds": map[string]any{"type": "number", "description": "Seconds to wait for a fast-failing launch before returning task_id (default 2, 0=immediate)."},
				},
			},
		},
	}
}

func builtinsForMode(mode Mode, hasTools, hasDynamicCatalog bool) []llm.ToolSpec {
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
	discovery := func() []llm.ToolSpec {
		return pick("search_tools", "get_tool", "execute_tool", "run_parallel", "background_run")
	}
	switch mode {
	case ModeCompactDirect:
		if hasDynamicCatalog {
			return discovery()
		}
		return pick("get_tool", "execute_tool", "run_parallel", "background_run")
	case ModeDiscovery:
		return discovery()
	default:
		if hasDynamicCatalog {
			return discovery()
		}
		return pick("run_parallel", "background_run")
	}
}

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

func AgentToolSpec() []llm.ToolSpec {
	specs := []llm.ToolSpec{
		{
			Name: "agent_spawn.agent",
			Description: "Delegate to specialist sub-agents (coordinator role only).\n\n" +
				"MODES:\n" +
				"  (1) spawn one:   agent(agent=<id>, task=...) → run_id\n" +
				"  (2) spawn many:  agent(agents=[{agent,task}, ...]) → {run_ids, count}\n" +
				"      Both modes are NON-BLOCKING by default. Add wait=true to block until done.\n" +
				"  (3) collect one: agent(run_id=..., wait=true) → finished snapshot\n" +
				"  (4) collect all: agent(run_ids=[...], wait=true) → {agents:[...]}\n" +
				"  (5) status:      agent(run_id=...) — avoid, use wait=true instead\n" +
				"  (6) list:        agent(list=true) → {agents:[...]}\n" +
				"  (7) cancel:      agent(cancel=true, run_id=...)\n\n" +
				"FAN-OUT (recommended for parallel work):\n" +
				"  agent(agents=[{agent:\"x\",task:\"...\"},{agent:\"y\",task:\"...\"}]) → run_ids\n" +
				"  agent(run_ids=[r1,r2], wait=true) → all results at once\n\n" +
				"Each sub-agent runs in full isolation. run_ids are agent runs.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"agent":       map[string]any{"type": "string", "description": "Target specialist agent id (mode 1)."},
					"agents":      map[string]any{"type": "array", "description": "Batch spawn: [{agent,task,memory_seed?}, ...] (mode 2).", "items": map[string]any{"type": "object"}},
					"task":        map[string]any{"type": "string", "description": "Instruction for the sub-agent (modes 1)."},
					"memory_seed": map[string]any{"type": "string", "description": "Read-only context to brief the sub-agent (modes 1,2)."},
					"run_id":      map[string]any{"type": "string", "description": "Existing sub-agent run id (modes 3,5,7)."},
					"run_ids":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Multiple run ids to collect (mode 4)."},
					"wait":        map[string]any{"type": "boolean", "description": "Block until sub-agent(s) finish. Default false."},
					"timeout":     map[string]any{"type": "number", "description": "Wait timeout in seconds."},
					"list":        map[string]any{"type": "boolean", "description": "List all sub-agents (mode 6)."},
					"cancel":      map[string]any{"type": "boolean", "description": "Cancel run_id and its subtree (mode 7)."},
				},
			},
		},
		{
			Name: "context_builder.kv",
			Description: "Shared key-value store for all agents in the same session tree.\n\n" +
				"Agents running in parallel can exchange discoveries without blocking each other.\n\n" +
				"MODES:\n" +
				"  write:  kv(key=\"x\", value=\"v\") → {written:true}\n" +
				"  read:   kv(key=\"x\")            → {key,value,found}\n" +
				"  delete: kv(key=\"x\", delete=true)\n" +
				"  list:   kv(list=true)           → {entries:{key:value,...}}",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"key":    map[string]any{"type": "string", "description": "Key to read or write."},
					"value":  map[string]any{"type": "string", "description": "Value to write (set mode)."},
					"delete": map[string]any{"type": "boolean", "description": "Delete the key."},
					"list":   map[string]any{"type": "boolean", "description": "Return all entries."},
				},
			},
		},
	}
	for i := range specs {
		specs[i].Name = sanitizeToolName(specs[i].Name)
	}
	return specs
}
