package runtime

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/context/prompt"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// appMemoryEnabled reports whether the app opted into the `memory` module, per
// the documented contract (docs-site/docs/reference/modules/memory.md +
// language/04b-builtin-tools.md "Memory tools (gated by tools.modules.memory)").
// The memory module is activated like any other module : by DECLARING it under
// tools.modules (or the top-level modules: map). Presence = enabled — there is
// no `working_memory` sub-flag in the doc. An app that didn't declare it never
// sees the memory tools, the instructions, or the snapshot.
func appMemoryEnabled(app *appmgr.RuntimeApp) bool {
	return appModuleDeclared(app, "memory")
}

// appAgentSpawnEnabled reports whether the app loaded the `agent_spawn` module,
// per docs-site/docs/reference/modules/agent_spawn.md +
// language/04b-builtin-tools.md "Agent-spawn tool (gated by agent_spawn module
// loaded)". The module is loaded either by declaring it under tools.modules /
// modules:, OR by granting it in tools.capabilities.grant ({module: agent_spawn}).
// Only then is the `agent_spawn.agent` delegation tool injected (a second
// coordinator-role gate still applies at dispatch time).
func appAgentSpawnEnabled(app *appmgr.RuntimeApp) bool {
	if appModuleDeclared(app, "agent_spawn") {
		return true
	}
	if app == nil || app.Definition == nil || app.Definition.Tools == nil {
		return false
	}
	caps := app.Definition.Tools.Capabilities
	if caps == nil {
		return false
	}
	for _, g := range caps.Grant {
		if g.Module == "agent_spawn" {
			return true
		}
	}
	return false
}

// primitiveAvailability is satisfied by the MetaDispatcher. It reports which
// optional context_builder primitive bridges are wired, so the engine offers
// each one only when it can actually run. Bool-only methods so the runtime
// package needn't import the meta package (avoids an import cycle).
type primitiveAvailability interface {
	CallAppWired() bool
	AskUserWired() bool
	UseSkillWired() bool
}

// appGrantsAskUser reports whether the app granted context_builder.ask_user via
// tools.capabilities.grant — the documented contract for exposing ask_user
// (docs-site/docs/reference/modules/context_builder.md). An empty actions list
// on a context_builder grant means "all actions" and also enables it.
func appGrantsAskUser(app *appmgr.RuntimeApp) bool {
	if app == nil || app.Definition == nil || app.Definition.Tools == nil {
		return false
	}
	caps := app.Definition.Tools.Capabilities
	if caps == nil {
		return false
	}
	for _, g := range caps.Grant {
		if g.Module != "context_builder" {
			continue
		}
		tools := g.EffectiveTools()
		if len(tools) == 0 {
			return true
		}
		for _, t := range tools {
			if t == "ask_user" {
				return true
			}
		}
	}
	return false
}

// appHasSkills reports whether any skill is declared that use_skill could load —
// top-level dev.skills or the agent's own capabilities.skills.
func appHasSkills(app *appmgr.RuntimeApp, agent *schema.Agent) bool {
	if app != nil && app.Definition != nil && app.Definition.Dev != nil && len(app.Definition.Dev.Skills) > 0 {
		return true
	}
	if agent != nil && len(agent.Capabilities.Skills) > 0 {
		return true
	}
	return false
}

// appModuleDeclared reports whether moduleID is declared in either the
// tools.modules map or the top-level modules: map. Declaration = activation for
// a module (the documented opt-in), regardless of any config sub-keys.
func appModuleDeclared(app *appmgr.RuntimeApp, moduleID string) bool {
	if app == nil || app.Definition == nil {
		return false
	}
	def := app.Definition
	if _, ok := def.ModulesTop[moduleID]; ok {
		return true
	}
	if def.Tools != nil {
		if _, ok := def.Tools.Modules[moduleID]; ok {
			return true
		}
	}
	return false
}

// workingMemoryView maps the live session snapshot into the prompt-layer view
// the assembler renders. Built fresh every turn from durable state.
func workingMemoryView(snap sessionstore.SessionSnapshot) *prompt.WorkingMemoryView {
	wm := &prompt.WorkingMemoryView{Goal: snap.Goal, Facts: snap.Facts}
	for _, t := range snap.Todos {
		wm.Todos = append(wm.Todos, prompt.TodoLine{ID: t.ID, Text: t.Text, Status: t.Status})
	}
	// CurrentQuestion comes from snap.LastUserMessage — persisted in the snapshot,
	// survives compaction, always reflects the current user request.
	if q := strings.TrimSpace(snap.LastUserMessage); q != "" {
		wm.CurrentQuestion = q
	}
	// NeedsGoal: the agent has active (non-done) todos but no goal.
	// Triggers a directive to call memory.set_goal before any other action.
	if snap.Goal == "" {
		for _, t := range snap.Todos {
			if t.Status != "done" && t.Status != "completed" {
				wm.NeedsGoal = true
				break
			}
		}
	}
	return wm
}

// The engine is the single durable home for working memory : every mutation is
// ONE session event (no side KV store — the split-brain bug the Python memory
// had). State reads assign task ids and report dedup ; secrets are redacted
// from facts before they're persisted. These four methods satisfy
// meta.MemoryWriter.

// SetGoal records the session objective. Surfaced in working memory every turn
// (survives compaction + resume) ; carried on the message content per the
// EventGoalSet projection.
func (e *Engine) SetGoal(ctx context.Context, sessionID, appID, userID, goal string) error {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return fmt.Errorf("goal is empty")
	}
	_, err := e.Sessions.AppendDurable(ctx, sessionstore.Event{
		Type:      sessionstore.EventGoalSet,
		SessionID: sessionID, AppID: appID, UserID: userID,
		Message: &sessionstore.MessagePayload{Role: "system", Content: goal},
	})
	return err
}

// Remember stores a durable fact. Secrets are redacted first ; dedup is reported
// to the LLM (the projection also dedups, so a slipped duplicate is still a
// no-op — this read just lets us say "already remembered").
func (e *Engine) Remember(ctx context.Context, sessionID, appID, userID, content string) (string, bool, error) {
	content = redactSecretsInText(strings.TrimSpace(content))
	if content == "" {
		return "", false, fmt.Errorf("content is empty")
	}
	if st, err := e.Sessions.State(sessionID); err == nil && st != nil {
		needle := strings.ToLower(content)
		for _, f := range st.Snapshot().Facts {
			if strings.ToLower(strings.TrimSpace(f)) == needle {
				return "", true, nil
			}
		}
	}
	_, err := e.Sessions.AppendDurable(ctx, sessionstore.Event{
		Type:      sessionstore.EventMemoryFactAdded,
		SessionID: sessionID, AppID: appID, UserID: userID,
		Memory: &sessionstore.MemoryPayload{Fact: content},
	})
	return "", false, err
}

// TaskCreate appends a task with an engine-assigned id (t1, t2, …).
func (e *Engine) TaskCreate(ctx context.Context, sessionID, appID, userID, subject, description string) (string, string, []sessionstore.Todo, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return "", "", nil, fmt.Errorf("subject is empty")
	}
	content := subject
	if d := strings.TrimSpace(description); d != "" {
		content = subject + ": " + d
	}
	id := e.nextTaskID(sessionID)
	if _, err := e.Sessions.AppendDurable(ctx, sessionstore.Event{
		Type:      sessionstore.EventTodoAdded,
		SessionID: sessionID, AppID: appID, UserID: userID,
		Todo: &sessionstore.TodoPayload{ID: id, Text: content, Status: "pending"},
	}); err != nil {
		return "", "", nil, err
	}
	return id, content, e.todosSnapshot(sessionID), nil
}

// todosSnapshot reads the session's current task list (post-mutation), tolerant
// of a missing/unreadable state (returns nil — the caller still has its ack).
func (e *Engine) todosSnapshot(sessionID string) []sessionstore.Todo {
	if st, err := e.Sessions.State(sessionID); err == nil && st != nil {
		return st.Snapshot().Todos
	}
	return nil
}

// TaskUpdate moves a task to a new status. The task must exist (a clear error
// beats a silent no-op the LLM can't see).
func (e *Engine) TaskUpdate(ctx context.Context, sessionID, appID, userID, taskID, status string) ([]sessionstore.Todo, error) {
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case "pending", "in_progress", "completed", "done", "blocked":
	default:
		return nil, fmt.Errorf("invalid status %q (use pending|in_progress|completed|blocked)", status)
	}
	found := false
	if st, err := e.Sessions.State(sessionID); err == nil && st != nil {
		for _, t := range st.Snapshot().Todos {
			if t.ID == taskID {
				found = true
				break
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("task %q not found", taskID)
	}
	if _, err := e.Sessions.AppendDurable(ctx, sessionstore.Event{
		Type:      sessionstore.EventTodoUpdated,
		SessionID: sessionID, AppID: appID, UserID: userID,
		Todo: &sessionstore.TodoPayload{ID: taskID, Status: status},
	}); err != nil {
		return nil, err
	}
	return e.todosSnapshot(sessionID), nil
}

// nextTaskID returns the next free "t<N>" id by scanning existing tasks.
func (e *Engine) nextTaskID(sessionID string) string {
	v, _ := e.taskSeq.LoadOrStore(sessionID, new(int64))
	ctr := v.(*int64)
	// Seed the counter from durable state the first time it is used for this
	// session (cold start / after a restart), so ids resume past any persisted
	// task instead of restarting at t1. CompareAndSwap makes the seed safe under
	// concurrent first-use ; from then on AddInt64 hands out unique ids even
	// when a batch of task_create runs in parallel.
	if atomic.LoadInt64(ctr) == 0 {
		max := 0
		if st, err := e.Sessions.State(sessionID); err == nil && st != nil {
			for _, t := range st.Snapshot().Todos {
				if strings.HasPrefix(t.ID, "t") {
					if n, err := strconv.Atoi(t.ID[1:]); err == nil && n > max {
						max = n
					}
				}
			}
		}
		atomic.CompareAndSwapInt64(ctr, 0, int64(max))
	}
	return fmt.Sprintf("t%d", atomic.AddInt64(ctr, 1))
}

// secretPatterns are HIGH-CONFIDENCE secret shapes. Value-based (the secret is
// in the text), unlike the Python redactor which only matched env-var values by
// key name — so a secret the agent typed slipped through. We deliberately avoid
// generic "long hex/base64" rules : they'd shred legitimate facts (git SHAs,
// hashes, paths) the agent wants to keep.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{6,}\.[A-Za-z0-9_\-]{6,}\.[A-Za-z0-9_\-]{6,}`),                                    // JWT
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{12,}`),                                                                // bearer token
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),                                                                                 // AWS access key id
	regexp.MustCompile(`(?i)(api[_\-]?key|secret|token|password|passwd|private[_\-]?key|access[_\-]?key)\s*[:=]\s*\S{6,}`), // key=value
	regexp.MustCompile(`sk-[A-Za-z0-9]{16,}`),                                                                              // OpenAI-style key
	regexp.MustCompile(`ghp_[A-Za-z0-9]{20,}`),                                                                             // GitHub PAT
}

func redactSecretsInText(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}
