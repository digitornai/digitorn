package prompt

import (
	"fmt"
	"regexp"
	"strings"
)

// directiveTag matches the opening of any control tag the supervising runtime
// owns — <digitorn-directive ...>, <digitorn-protocol ...> and their closers —
// case-insensitively, tolerating stray whitespace. Compiled ONCE (this renders
// every turn, so no per-call regexp.Compile).
var directiveTag = regexp.MustCompile(`(?i)<(\s*/?\s*digitorn-)`)

// neutralizeDirectives makes agent-writable text unable to forge a runtime
// supervisor directive : it escapes the '<' of any <digitorn-*> tag so the model
// sees inert literal text (&lt;digitorn-...), never a parseable control command.
// Working memory is writable by the agent (memory.remember / set_goal) and can
// derive from untrusted tool output, so every memory string MUST pass through
// here before it lands in the system prompt next to the REAL directives.
func neutralizeDirectives(s string) string {
	if !strings.Contains(strings.ToLower(s), "digitorn-") {
		return s // fast path : nothing that could impersonate a directive
	}
	return directiveTag.ReplaceAllString(s, "&lt;$1")
}

// WorkingMemoryView is the minimal slice of session state the memory snapshot
// renders into the system prompt each turn. The engine maps the live session
// snapshot into it ; keeping it a plain view (no sessionstore import) makes the
// renderer pure and unit-testable.
type WorkingMemoryView struct {
	Goal  string
	Todos []TodoLine
	Facts []string
}

// TodoLine is one task as the snapshot shows it.
type TodoLine struct {
	ID     string
	Text   string
	Status string // pending | in_progress | done (alias: completed) | blocked
}

func (wm WorkingMemoryView) empty() bool {
	return wm.Goal == "" && len(wm.Todos) == 0 && len(wm.Facts) == 0
}

// memoryFactsShown caps how many key facts the snapshot renders (most recent
// first-wins on overflow) so the injected block stays within a sane budget.
const memoryFactsShown = 12

// RenderWorkingMemory renders the agent's durable working memory — goal, task
// progress, key facts — as a compact text block. It is re-rendered from durable
// state EVERY turn, so it survives context compaction AND session resume : the
// agent always sees its objective and where it left off. Returns "" when
// there's nothing to show.
func RenderWorkingMemory(wm WorkingMemoryView) string {
	if wm.empty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("Working memory (durable — survives compaction and resume):")
	if wm.Goal != "" {
		b.WriteString("\nGoal: ")
		b.WriteString(neutralizeDirectives(wm.Goal))
	}
	if len(wm.Todos) > 0 {
		done := 0
		for _, t := range wm.Todos {
			if todoDone(t.Status) {
				done++
			}
		}
		fmt.Fprintf(&b, "\nTasks (%d/%d done):", done, len(wm.Todos))
		nextShown := false
		for _, t := range wm.Todos {
			line := fmt.Sprintf("\n  %s [%s] %s", todoMark(t.Status), t.ID, neutralizeDirectives(t.Text))
			if todoPending(t.Status) && !nextShown {
				line += "  <- next"
				nextShown = true
			}
			b.WriteString(line)
		}
	}
	if len(wm.Facts) > 0 {
		b.WriteString("\nKey facts:")
		start := 0
		if len(wm.Facts) > memoryFactsShown {
			start = len(wm.Facts) - memoryFactsShown
		}
		for _, f := range wm.Facts[start:] {
			b.WriteString("\n  - ")
			b.WriteString(neutralizeDirectives(f))
		}
	}
	return b.String()
}

// memoryInstructions is the static how-to block injected when the agent has the
// memory tools available. Deliberately terse — the doc-conform 4-tool surface,
// no cognitive-overload of the Python version's aspirational layers.
const memoryInstructions = `## Working memory & task plan
You have a durable working memory (shown above) that survives context compaction and session resume. Keep it current.

- memory.set_goal(goal="...") — set the one-line session objective as soon as the user's intent is clear.
- memory.remember(content="...") — store a durable fact you'll need later (a key finding, an exact command, a path, a decision). Survives compaction; secrets are auto-redacted.

### Plan with tasks — this is how you stay reliable
memory.task_create / memory.task_update are your plan and your progress tracker. The task list is a contract with the user: it shows the plan, proves you followed it, and guarantees nothing is forgotten.

CREATE tasks when (do it every time):
- the request needs MORE THAN TWO distinct steps, OR spans multiple files/areas, OR has phases (investigate → change → verify).
- Lay out the WHOLE plan as a batch (call task_create ≥2 times) UP FRONT, BEFORE acting. One task per concrete, verifiable step — specific ("add the /login route", "wire auth middleware", "add a login test"), never vague ("fix the app").

DON'T create tasks when:
- it's a single trivial step, a question, an explanation, or a one-line change. Never make a task per question or wrap obvious one-shot work in ceremony — a one-item list is noise.

EXECUTE the plan — follow it, don't drift:
- Keep EXACTLY ONE task in_progress at a time: task_update(status="in_progress") the moment you start it.
- The INSTANT a step is done, task_update(status="completed"). Update in real time — never batch updates, never mark done before it truly is.
- Do the tasks in order. Discover a new required step → task_create it. A task became unnecessary → complete/blocked it with a note. Keep the list matching reality.
- A step that can't proceed (missing input, failure) → task_update(status="blocked") and say why.

FINISH the plan before you stop (hard rule):
- Do NOT end your turn while any task is still pending or in_progress — that means the work isn't done. Re-read the list, finish every remaining task, then stop. Every task must end completed (or blocked with a reason). The runtime reads in_progress tasks to resume an interrupted turn, so honest, up-to-date statuses are mandatory.`

func todoDone(s string) bool    { return s == "done" || s == "completed" }
func todoPending(s string) bool { return s == "" || s == "pending" }

func todoMark(s string) string {
	switch s {
	case "done", "completed":
		return "[x]"
	case "in_progress":
		return "[~]"
	case "blocked":
		return "[!]"
	default:
		return "[ ]"
	}
}
