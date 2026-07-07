package catalog

import (
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/pkg/module"
)

const askUserPrompt = `ask_user is HOW YOU TALK TO THE USER while you work. It is not a last resort — it is your live conversation channel. Use it literally to speak to them: to clarify what they meant, to propose options, to confirm before a risky step, to collect the details you need, to report a fork and let them choose. Calling it does NOT end the turn — it suspends the turn until they reply, then you continue with their answer. So you can have a real back-and-forth WITHOUT stopping. When in doubt, ALWAYS ask.

THE RULE — default to asking. You are NOT here to take initiative on decisions the user has not made; you are here to do exactly what they want, and the only way to know is to ask. If you are not certain what they want, do not act on a guess and do not end the turn — ask.

Ask BEFORE you act whenever ANY of these is true:
- the request is ambiguous or can be read more than one way, or you are filling in a blank the user left;
- you are about to pick a name, path, location, structure, format, library, version, or convention they did not specify;
- there are several valid approaches and you would be choosing one of them FOR the user;
- the step is consequential or hard to undo — deleting, overwriting, migrating, deploying, spending, sending, or anything touching real / production data;
- you need a fact only the user has — which target, which environment, which account, which branch, a business rule, a credential choice;
- you hit an error, a surprise, or a fork and the right next move depends on what they intended;
- you catch yourself about to write "I'll assume…", "probably…", "I'll just go with…", or to leave a TODO and move on.

NEVER do these — ask instead:
- never invent a value (name, id, path, version) and proceed as if the user had chosen it;
- never silently pick one of several valid approaches;
- never run a destructive or irreversible action on details you inferred;
- never end a turn with an assumption baked in, or work left half-done because you were unsure — the turn can wait for an answer, so get the answer.

Do NOT, however, ask what you can answer yourself from the repo, the conversation, or your tools — find it first. Do not ask permission for a clearly-authorized, low-risk, reversible step. When several things are unclear, ask them TOGETHER in one form, not as a string of separate questions.

Pick the shape that fits the decision. Whenever you PROPOSE options, keep allow_custom on (the default) so the user can give an answer you did not foresee — they always know their intent better than your option list.

Free text — ask for a value or an open answer:
  {"question": "What should the new service be called?"}
  {"question": "What's the base URL of the API I should call?", "placeholder": "https://api.example.com"}

Disambiguate intent — when the request itself is unclear, ask which reading is right:
  {"question": "\"Clean up the auth module\" — what do you mean?", "choices": ["Just reformat / lint", "Refactor for readability", "Remove dead code", "Fix the known bugs"]}

Choose between approaches — when several designs are valid, let the user pick:
  {"question": "Two ways to add caching here — which do you prefer?", "choices": ["In-memory (simple, per-process)", "Redis (shared, needs a server)"]}

Mid-task checkpoint — report what you found and let them steer:
  {"question": "I found 3 failing tests: 1 is a real bug, 2 look flaky. What should I fix?", "choices": ["Just the real bug", "All three", "Show me the details first"]}

Single choice — a few exclusive options (the user can still type their own):
  {"question": "Which database should I wire up?", "choices": ["PostgreSQL", "MySQL", "SQLite"]}

Multi-select — several picks at once:
  {"question": "Which features should I scaffold?", "choices": ["Auth", "Billing", "Admin", "API docs"], "allow_multiple": true}

Strict choice — no free-form answer (rare; a hard gate before something risky):
  {"question": "Proceed with deleting these 12 files?", "choices": ["Yes", "No"], "allow_custom": false}
  {"question": "Which environment should I deploy to?", "choices": ["staging", "production"], "allow_custom": false}

Editable review — hand the user a draft, plan, or text to edit in place and approve:
  {"question": "Review this migration plan before I run it:", "content": "1. add column email\n2. backfill from profile\n3. drop legacy column"}
  {"question": "Here's the commit message — edit it however you like:", "content": "feat(auth): add refresh-token rotation"}

Form — gather several related answers in one go (each answer is keyed by the field name):
  {"question": "Let's configure the project.", "form": [
    {"name": "name", "label": "Project name", "type": "text", "required": true},
    {"name": "framework", "label": "Framework", "type": "select", "options": ["FastAPI", "Django", "Flask"], "default": "FastAPI"},
    {"name": "features", "label": "Features", "type": "multiselect", "options": ["Auth", "DB", "Tests", "CI"]},
    {"name": "workers", "label": "Worker count", "type": "integer", "default": "4", "min": 1, "max": 32},
    {"name": "docker", "label": "Add Docker?", "type": "boolean", "default": true}
  ]}

Complex form — a deeper example (deployment settings):
  {"question": "Configure the deployment.", "form": [
    {"name": "env", "label": "Environment", "type": "select", "options": ["dev", "staging", "prod"], "required": true},
    {"name": "region", "label": "Region", "type": "select", "options": ["us-east", "eu-west", "ap-south"], "default": "eu-west"},
    {"name": "replicas", "label": "Replicas", "type": "integer", "default": "2", "min": 1, "max": 10},
    {"name": "domains", "label": "Domains", "type": "multiselect", "options": ["app", "api", "admin"]},
    {"name": "rollback", "label": "Auto-rollback on failure?", "type": "boolean", "default": true},
    {"name": "notes", "label": "Release notes", "type": "textarea", "placeholder": "what changed"}
  ]}

Every field type — number, date, email, url, password, range, rating — with validation (required, pattern, min/max, min_length):
  {"question": "Account setup.", "form": [
    {"name": "email", "label": "Email", "type": "email", "required": true},
    {"name": "website", "label": "Website", "type": "url", "placeholder": "https://"},
    {"name": "password", "label": "Password", "type": "password", "min_length": 8},
    {"name": "birthday", "label": "Birthday", "type": "date"},
    {"name": "budget", "label": "Monthly budget ($)", "type": "number", "min": 0, "max": 10000},
    {"name": "volume", "label": "Volume", "type": "range", "min": 0, "max": 100, "step": 5, "default": "50"},
    {"name": "rating", "label": "Rate us", "type": "rating", "max": 5},
    {"name": "username", "label": "Username", "type": "text", "pattern": "^[a-z0-9_]{3,20}$", "required": true}
  ]}

Bounded multi-select — require between a minimum and a maximum number of picks:
  {"question": "Pick 2 to 3 priorities for this sprint:", "choices": ["Perf", "Security", "UX", "Docs", "Tests"], "allow_multiple": true, "min_select": 2, "max_select": 3}

Bottom line: in doubt → ask. It is never wrong to ask, and it is usually wrong to assume.`


const askUserToolPrompt = `ask_user is your live channel to TALK TO THE USER while you work — USE IT, and use it often. Clarify, propose, confirm, and collect input through it instead of guessing or ending the turn. Default to asking over guessing: the MOMENT the request is ambiguous, a detail is unspecified, several valid approaches exist, a step is risky or irreversible, or only the user holds a needed fact — STOP and call ask_user. Asking does NOT end the turn; it pauses for the answer, then you continue, so you can have a real back-and-forth. Never invent a name/path/value and run with it, never silently pick one approach, never finish a turn on "I'll assume…". Skip asking only when you can find the answer yourself or the step is clearly authorized and low-risk. Batch several unknowns into ONE form, and when you offer choices keep the custom-answer field on so the user can answer what you didn't anticipate.`

func systemModuleManifests() []module.Manifest {
	low := func(name, desc string, params ...tool.ParamSpec) tool.Spec {
		return tool.Spec{Name: name, Description: desc, RiskLevel: tool.RiskLow, Params: params}
	}
	str := func(name, desc string, required bool) tool.ParamSpec {
		return tool.ParamSpec{Name: name, Type: "string", Description: desc, Required: required}
	}
	strOpt := func(name, desc string) tool.ParamSpec {
		return tool.ParamSpec{Name: name, Type: "string", Description: desc}
	}
	boolp := func(name, desc string) tool.ParamSpec {
		return tool.ParamSpec{Name: name, Type: "boolean", Description: desc}
	}
	intp := func(name, desc string) tool.ParamSpec {
		return tool.ParamSpec{Name: name, Type: "integer", Description: desc}
	}
	nump := func(name, desc string) tool.ParamSpec {
		return tool.ParamSpec{Name: name, Type: "number", Description: desc}
	}
	strArr := func(name, desc string) tool.ParamSpec {
		return tool.ParamSpec{Name: name, Type: "array", Description: desc, Items: &tool.ParamSpec{Type: "string"}}
	}

	askForm := tool.ParamSpec{
		Name: "form", Type: "array",
		Description: "Structured form — one object per field; the answer is a JSON object keyed by each field's `name`.",
		Items: &tool.ParamSpec{Type: "object", Properties: []tool.ParamSpec{
			str("name", "Field key in the answer JSON.", true),
			strOpt("label", "Display label (defaults to name)."),
			{Name: "type", Type: "string", Description: "Field control.",
				Enum: []any{"text", "textarea", "number", "integer", "boolean", "select", "multiselect", "email", "url", "date", "password", "range", "rating"}},
			strOpt("description", "Help text shown under the field."),
			strOpt("placeholder", "Hint shown in an empty input."),
			strOpt("default", "Pre-filled value."),
			boolp("required", "The user must answer this field."),
			strArr("options", "Choices for select / multiselect."),
			boolp("allow_custom", "select/multiselect : allow a typed answer not in options (default true)."),
			nump("min", "Minimum for number / range / rating."),
			nump("max", "Maximum for number / range / rating."),
			nump("step", "Step for range."),
			intp("min_length", "Minimum text length."),
			intp("max_length", "Maximum text length."),
			strOpt("pattern", "Regex the text answer must match."),
		}},
	}

	askUser := low("ask_user", askUserPrompt,
		str("question", "The question to put to the user.", true),
		strArr("choices", "Proposed options to pick from (renders as buttons / a dropdown)."),
		boolp("allow_multiple", "With choices : the user may pick several."),
		boolp("allow_custom", "With proposals : let the user type an answer you didn't offer (default true)."),
		intp("min_select", "Multi-select : minimum number of picks."),
		intp("max_select", "Multi-select : maximum number of picks."),
		strOpt("content", "Markdown for the user to review and edit in place."),
		strOpt("default", "Pre-filled answer for a text / single-choice question."),
		strOpt("placeholder", "Hint shown in an empty text input."),
		boolp("multiline", "The text answer spans multiple lines."),
		askForm,
		nump("timeout", "Seconds to wait for an answer (0 = default 300s)."),
	)
	askUser.ToolPrompt = askUserToolPrompt
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

			ID:          "context_builder",
			Version:     "1.0.0",
			Description: "Tool discovery + human-in-the-loop primitives (search_tools, get_tool, execute_tool, run_parallel, background_run, use_skill, call_app, ask_user).",
			Tools: []tool.Spec{
				low("search_tools", "Discover tools by query / category."),
				low("get_tool", "Fetch one tool's full schema."),
				low("execute_tool", "Execute a discovered tool by name."),
				low("run_parallel", "Run several tool calls concurrently."),
				low("background_run",
					"Launch / monitor / cancel a background task. Five modes: "+
						"(1) LAUNCH: {name, params, settle_seconds?} → returns {task_id, settled?}; "+
						"(2) STATUS: {task_id, tail_lines?} → returns {state, log, log_lines, result?, error?}; "+
						"(3) WAIT: {task_id, wait:true, timeout?, tail_lines?} → blocks until done; "+
						"(4) LIST: {list_tasks:true} → all tasks for this session; "+
						"(5) CANCEL: {task_id, cancel:true} → kill the task tree. "+
						"The `log` field is the LIVE TAIL of the task's stdout+stderr (sliced to the last `tail_lines` lines, default 100; pass tail_lines:0 to get the full 64KB window). Use it to spot startup errors, watch a build progress, or confirm a server is serving. The `log_lines` field tells you how many lines you got back.",
					strOpt("name", "Tool to launch in the background (launch mode only)."),
					tool.ParamSpec{Name: "params", Type: "object", Description: "Args for the launched tool (launch mode)."},
					strOpt("task_id", "Task id returned by an earlier launch (status / wait / cancel)."),
					boolp("list_tasks", "List every task in this session."),
					boolp("wait", "Block until the task finishes (combine with task_id)."),
					boolp("cancel", "Kill the task tree (combine with task_id)."),
					tool.ParamSpec{Name: "timeout", Type: "number", Description: "Seconds to wait when wait:true (0 = no timeout)."},
					tool.ParamSpec{Name: "settle_seconds", Type: "number", Description: "On launch, hold for N seconds to catch fast-failing startup. 0 disables. Default ~2s."},
					tool.ParamSpec{Name: "tail_lines", Type: "integer", Description: "Last N lines of live output to return on status / wait. Default 100; 0 = full 64KB window."},
				),
				low("use_skill", "Load a /command skill's instructions."),
				low("call_app", "Invoke another deployed app as a sub-tool."),
				askUser,
			},
		},
		{
	
			ID:          "channels",
			Version:     "1.0.0",
			Description: "Background channels & schedules (inbound adapters + declarative cron with message/reports/attachments) — read by the background service.",
		},
	}
}
