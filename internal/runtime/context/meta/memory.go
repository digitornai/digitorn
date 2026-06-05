package meta

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// MemoryWriter persists the agent's durable working-memory mutations as session
// events (a SINGLE event-sourced path — no side KV store, unlike the Python
// reference). The daemon wires the engine here.
//
// All methods are session-scoped : memory lives in the caller's own session
// (a sub-agent's memory is in its sub-session), so isolation is automatic. The
// engine reads current state to assign task ids and redacts secrets before
// persisting a fact.
type MemoryWriter interface {
	SetGoal(ctx context.Context, sessionID, appID, userID, goal string) error
	// Remember stores a durable fact ; deduped=true means an equivalent fact
	// was already present (no-op). Secrets are redacted by the implementation.
	Remember(ctx context.Context, sessionID, appID, userID, content string) (id string, deduped bool, err error)
	// TaskCreate appends a task and returns its assigned id (t1, t2, …) plus the
	// FULL task list after the append, so the tool can hand the agent its updated
	// plan in the result.
	TaskCreate(ctx context.Context, sessionID, appID, userID, subject, description string) (id, content string, todos []sessionstore.Todo, err error)
	// TaskUpdate moves a task to a new status (pending|in_progress|completed|blocked)
	// and returns the FULL task list after the change, so the tool can re-project
	// the plan (next task, what's left) back to the agent.
	TaskUpdate(ctx context.Context, sessionID, appID, userID, taskID, status string) (todos []sessionstore.Todo, err error)
}

// handleSetGoal : set the session's objective. Surfaced in working memory every
// turn (survives compaction + resume).
func (m *MetaDispatcher) handleSetGoal(ctx context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	if m.Memory == nil {
		return errored("memory not wired (no MemoryWriter)")
	}
	goal := firstString(call.Args, "goal")
	if goal == "" {
		return errored("set_goal: 'goal' is required")
	}
	if err := m.Memory.SetGoal(ctx, call.SessionID, call.AppID, call.UserID, goal); err != nil {
		return errored("set_goal: " + err.Error())
	}
	return jsonOutcome(map[string]any{"goal": goal})
}

// handleRemember : store a durable fact. Dedup is reported back so the LLM
// knows whether it was a no-op.
func (m *MetaDispatcher) handleRemember(ctx context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	if m.Memory == nil {
		return errored("memory not wired (no MemoryWriter)")
	}
	content := firstString(call.Args, "content", "fact")
	if content == "" {
		return errored("remember: 'content' is required")
	}
	id, deduped, err := m.Memory.Remember(ctx, call.SessionID, call.AppID, call.UserID, content)
	if err != nil {
		return errored("remember: " + err.Error())
	}
	out := map[string]any{"id": id, "stored": !deduped}
	if deduped {
		out["note"] = "already remembered (no-op)"
	}
	return jsonOutcome(out)
}

// handleTaskCreate : append a task to the session's working memory.
func (m *MetaDispatcher) handleTaskCreate(ctx context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	if m.Memory == nil {
		return errored("memory not wired (no MemoryWriter)")
	}
	subject := firstString(call.Args, "subject", "content")
	if subject == "" {
		return errored("task_create: 'subject' is required")
	}
	description, _ := call.Args["description"].(string)
	id, content, todos, err := m.Memory.TaskCreate(ctx, call.SessionID, call.AppID, call.UserID, subject, description)
	if err != nil {
		return errored("task_create: " + err.Error())
	}
	ack := fmt.Sprintf("Created %s (pending): %s", id, content)
	return textOutcome(ack + "\n\n" + planContext(todos))
}

// handleTaskUpdate : move a task to a new status (drives the UI + the resume
// protocol — the runtime reads in_progress tasks to continue an interrupted turn).
func (m *MetaDispatcher) handleTaskUpdate(ctx context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	if m.Memory == nil {
		return errored("memory not wired (no MemoryWriter)")
	}
	taskID := firstString(call.Args, "task_id", "taskId")
	status := firstString(call.Args, "status")
	if taskID == "" || status == "" {
		return errored("task_update: 'task_id' and 'status' are required")
	}
	todos, err := m.Memory.TaskUpdate(ctx, call.SessionID, call.AppID, call.UserID, taskID, status)
	if err != nil {
		return errored("task_update: " + err.Error())
	}
	ack := fmt.Sprintf("%s → %s.", taskID, statusLabel(status))
	return textOutcome(ack + "\n\n" + planContext(todos))
}

// textOutcome returns a plain-text tool result (the task tools speak to the
// agent in prose, not JSON — the todo UI is driven by the durable events, so
// the result is purely the agent's plan briefing).
func textOutcome(s string) runtime.ToolOutcome {
	return runtime.ToolOutcome{
		Status: "completed",
		Parts:  []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: s}},
	}
}

// statusLabel normalises a status for display ("" → pending).
func statusLabel(s string) string {
	if strings.TrimSpace(s) == "" {
		return "pending"
	}
	return s
}

// planContext re-projects the whole plan back to the agent after a task
// mutation : progress, the single NEXT task to focus on, everything still open,
// and a terse protocol nudge. This is the "powerful context" the agent gets on
// every task_create / task_update so it always knows where it stands and never
// drifts or stops with work outstanding.
func planContext(todos []sessionstore.Todo) string {
	if len(todos) == 0 {
		return "No tasks in the plan yet."
	}
	done, inProgress := 0, 0
	var firstInProgress, firstPending *sessionstore.Todo
	var remaining []string
	for i := range todos {
		t := &todos[i]
		switch t.Status {
		case "completed", "done", "ok", "success":
			done++
			continue
		case "in_progress", "running", "active":
			inProgress++
			if firstInProgress == nil {
				firstInProgress = t
			}
		case "pending", "":
			if firstPending == nil {
				firstPending = t
			}
		}
		remaining = append(remaining, "  • "+t.ID+" ["+statusLabel(t.Status)+"] "+t.Text)
	}
	total := len(todos)
	if len(remaining) == 0 {
		return fmt.Sprintf("Plan: %d/%d done. Every task is complete — you can finish the turn.", done, total)
	}
	next := firstInProgress
	if next == nil {
		next = firstPending
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Plan: %d/%d done.", done, total)
	if next != nil {
		fmt.Fprintf(&b, "\nNext → %s: %s", next.ID, next.Text)
	}
	b.WriteString("\nRemaining:\n" + strings.Join(remaining, "\n"))
	switch {
	case inProgress > 1:
		b.WriteString("\nYou have " + strconv.Itoa(inProgress) + " tasks in_progress — keep exactly ONE in flight.")
	case inProgress == 0 && firstPending != nil:
		b.WriteString("\nNothing is in_progress — mark " + firstPending.ID + " in_progress before working it.")
	}
	b.WriteString("\nMark each task completed the instant it's truly done; don't end your turn while any task is still open.")
	return b.String()
}
