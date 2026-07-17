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

func appMemoryEnabled(app *appmgr.RuntimeApp) bool {
	return appModuleDeclared(app, "memory")
}

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

type primitiveAvailability interface {
	CallAppWired() bool
	AskUserWired() bool
	UseSkillWired() bool
}

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

func appHasSkills(app *appmgr.RuntimeApp, agent *schema.Agent) bool {
	if app != nil && app.Definition != nil && app.Definition.Dev != nil && len(app.Definition.Dev.Skills) > 0 {
		return true
	}
	if agent != nil && len(agent.Capabilities.Skills) > 0 {
		return true
	}
	return false
}

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

func workingMemoryView(snap sessionstore.SessionSnapshot) *prompt.WorkingMemoryView {
	wm := &prompt.WorkingMemoryView{Goal: snap.Goal, Facts: snap.Facts}
	for _, t := range snap.Todos {
		wm.Todos = append(wm.Todos, prompt.TodoLine{ID: t.ID, Text: t.Text, Status: t.Status})
	}
	if q := strings.TrimSpace(snap.LastUserMessage); q != "" {
		wm.CurrentQuestion = q
	}
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

func (e *Engine) todosSnapshot(sessionID string) []sessionstore.Todo {
	if st, err := e.Sessions.State(sessionID); err == nil && st != nil {
		return st.Snapshot().Todos
	}
	return nil
}

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

func (e *Engine) nextTaskID(sessionID string) string {
	v, _ := e.taskSeq.LoadOrStore(sessionID, new(int64))
	ctr := v.(*int64)
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

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{6,}\.[A-Za-z0-9_\-]{6,}\.[A-Za-z0-9_\-]{6,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{12,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`(?i)(api[_\-]?key|secret|token|password|passwd|private[_\-]?key|access[_\-]?key)\s*[:=]\s*\S{6,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`ghp_[A-Za-z0-9]{20,}`),
}

func redactSecretsInText(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}
