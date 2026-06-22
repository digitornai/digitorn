package ctxinject

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/mbathepaul/digitorn/internal/runtime/context/repomap"
)

// Builtin renders a ready-made section body from the turn data. Returns "" to skip
// (e.g. no data to show).
type Builtin func(d Data) string

// builtins are the pre-configured, ready-to-use sections an app references by name
// (`builtin: datetime`). Domain-agnostic — they draw only from the generic data bag.
var builtins = map[string]Builtin{
	"datetime":     biDatetime,
	"date":         biDatetime,
	"user":         biUser,
	"session":      biSession,
	"identity":     biIdentity,
	"environment":  biEnv,
	"env":          biEnv,
	"code_index":   biCodeIndex,
	"memory_index": biMemoryIndex,
}

// BuiltinNames lists the available builtins (for validation / docs).
func BuiltinNames() []string {
	return []string{"datetime", "user", "session", "identity", "environment", "code_index", "memory_index"}
}

func biCodeIndex(d Data) string {
	root := toString(d.Session["workdir"])
	if root == "" {
		return ""
	}
	return repomap.Get(root)
}

// biMemoryIndex loads the project's working memory from .digitorn/memory/ and
// always emits the writing directive so the agent knows how and when to persist
// knowledge — even on the first turn when no files exist yet.
func biMemoryIndex(d Data) string {
	workdir := toString(d.Session["workdir"])
	if workdir == "" {
		return ""
	}
	var out strings.Builder
	memDir := filepath.Join(workdir, ".digitorn", "memory")
	if content := renderDirBudget(memDir); content != "" {
		out.WriteString("<system-reminder>\n")
		out.WriteString(content)
		out.WriteString("\n</system-reminder>\n\n")
	}
	out.WriteString(fileMemoryDirectiveFor(".digitorn/memory"))
	return out.String()
}

func fileMemoryDirectiveFor(dir string) string {
	if dir == "" {
		dir = ".digitorn/memory"
	}
	return "<digitorn-directive type=\"file_memory\" severity=\"normal\">\n" +
		"## Persistent file memory\n\n" +
		"You have a persistent memory in " + dir + "/. It is re-injected at the start of EVERY turn. " +
		"Always write in English. Use it aggressively — it is the only thing that survives context compaction and session restarts. " +
		"A fact not written here is a fact permanently lost.\n\n" +

		"**CREATE a memory file the moment you learn something reusable:**\n" +
		"- How the user works: communication style, level of detail they want, things they hate, things they love\n" +
		"  Examples: \"always show diffs before applying\", \"hates verbose explanations\", \"wants tests for every change\"\n" +
		"- Domain knowledge specific to this context: architecture decisions, business rules, key entities\n" +
		"  Examples (code): stack choices, test strategy, deploy process, naming conventions, forbidden patterns\n" +
		"  Examples (other): client preferences, workflow steps, recurring deadlines, key contacts, approval chains\n" +
		"- Constraints and non-negotiables: things to always or never do\n" +
		"  Examples: \"never delete without confirmation\", \"always cc legal on contracts\", \"branch names must be kebab-case\"\n" +
		"- Corrections and feedback the user gave you — these are the most important memories\n" +
		"  Examples: \"stop summarizing at the end\", \"you were wrong about X, the correct answer is Y\"\n" +
		"- Useful references: exact commands, paths, identifiers, URLs, names — anything you would forget\n" +
		"  Examples: \"deploy command: make deploy ENV=prod\", \"staging URL: https://staging.acme.com\"\n\n" +

		"**Do NOT write:**\n" +
		"- One-off task details that won't recur\n" +
		"- Things trivially visible in the current context\n" +
		"- Ephemeral state — use memory.task_create/task_update for in-progress work instead\n\n" +

		"**UPDATE an existing file when:**\n" +
		"- The user corrects or contradicts a stored fact (\"actually I changed my mind, now I prefer X\")\n" +
		"- New information makes the stored version incomplete or wrong\n" +
		"- A rule, preference, or constraint has evolved\n" +
		"Overwrite the whole file. One clear current version — no changelog, no history.\n\n" +

		"**DELETE a memory file when:**\n" +
		"- The user explicitly asks you to forget something\n" +
		"- The fact is provably obsolete and will never apply again\n" +
		"- A file has been fully absorbed into a more complete one\n" +
		"Always remove its entry from MEMORY.md too.\n\n" +

		"**File format** — use filesystem.write, always in English:\n\n" +
		"  Path: " + dir + "/<type>_<kebab-name>.md\n\n" +
		"  ---\n" +
		"  name: <kebab-slug>\n" +
		"  description: \"<one-line summary — used to judge relevance in future sessions>\"\n" +
		"  metadata:\n" +
		"    type: <user|feedback|project|reference>\n" +
		"  ---\n\n" +
		"  <Body: the fact itself, 1-5 sentences>\n\n" +
		"  **Why:** <the reason this matters or was stored>\n" +
		"  **How to apply:** <when and how to use this in future turns>\n\n" +

		"**MEMORY.md index** — always maintain " + dir + "/MEMORY.md:\n\n" +
		"  # Memory Index\n\n" +
		"  - [Title](filename.md) — one-line hook\n\n" +
		"  Add a line on create. Remove on delete. Keep it under 50 lines.\n\n" +

		"**Memory types:**\n" +
		"  user_*.md        — user preferences, expertise, working style\n" +
		"  feedback_*.md    — corrections and direct guidance from the user\n" +
		"  project_*.md     — domain facts, architecture, business logic, decisions\n" +
		"  reference_*.md   — commands, paths, URLs, identifiers, contacts\n" +
		"</digitorn-directive>"
}

func biDatetime(d Data) string {
	if d.Now.IsZero() {
		return ""
	}
	return fmt.Sprintf("Current date: %s (%s), local time %s. This is a snapshot taken at the start of the turn — it does not advance while you work.",
		d.Now.Format("2006-01-02"), d.Now.Weekday().String(), d.Now.Format("15:04"))
}

func biUser(d Data) string {
	get := func(k string) string { return toString(d.User[k]) }
	name := get("name")
	if name == "" {
		name = get("id")
	}
	if name == "" && len(d.User) == 0 {
		return ""
	}
	var b strings.Builder
	if name != "" {
		fmt.Fprintf(&b, "You are assisting %s.", name)
	} else {
		b.WriteString("You are assisting the user.")
	}
	if v := get("region"); v != "" {
		fmt.Fprintf(&b, " Region: %s.", v)
	}
	if v := get("locale"); v != "" {
		fmt.Fprintf(&b, " Locale: %s.", v)
	}
	if v := get("timezone"); v != "" {
		fmt.Fprintf(&b, " Timezone: %s.", v)
	}
	if v := get("email"); v != "" {
		fmt.Fprintf(&b, " Email: %s.", v)
	}
	if v := toString(d.User["roles"]); v != "" {
		fmt.Fprintf(&b, " Roles: %s.", v)
	}
	return b.String()
}

func biSession(d Data) string {
	get := func(k string) string { return toString(d.Session[k]) }
	var lines []string
	if v := get("goal"); v != "" {
		lines = append(lines, "Goal: "+v)
	}
	if v := get("mode"); v != "" {
		lines = append(lines, "Mode: "+v)
	}
	if v := get("turn"); v != "" && v != "0" {
		lines = append(lines, "Turn: "+v)
	}
	if v := get("workdir"); v != "" {
		lines = append(lines, "Working directory: "+v)
	}
	return strings.Join(lines, "\n")
}

func biEnv(d Data) string {
	get := func(k string) string { return toString(d.Env[k]) }
	platform, os, arch := get("platform"), get("os"), get("arch")
	if platform == "" && os == "" {
		return ""
	}
	var parts []string
	if platform != "" {
		parts = append(parts, "Platform: "+platform)
	}
	if os != "" {
		parts = append(parts, "OS: "+os)
	}
	if arch != "" {
		parts = append(parts, "Arch: "+arch)
	}
	if v := get("shell"); v != "" {
		parts = append(parts, "Shell: "+v)
	}
	return strings.Join(parts, " · ")
}

func biIdentity(d Data) string {
	app := toString(d.App["name"])
	if app == "" {
		app = toString(d.App["id"])
	}
	if app == "" {
		return ""
	}
	out := "You are running inside the " + app + " application."
	if v := toString(d.App["version"]); v != "" {
		out += " (version " + v + ")"
	}
	return out
}
