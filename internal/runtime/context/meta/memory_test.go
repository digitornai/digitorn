package meta_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/context/meta"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

type fakeMemory struct {
	goal       string
	remembered []string
	dedupNext  bool
	created    []string
	updated    map[string]string
	todos      []sessionstore.Todo
}

func (f *fakeMemory) SetGoal(_ context.Context, _, _, _, goal string) error {
	f.goal = goal
	return nil
}
func (f *fakeMemory) Remember(_ context.Context, _, _, _, content string) (string, bool, error) {
	if f.dedupNext {
		return "", true, nil
	}
	f.remembered = append(f.remembered, content)
	return "f1", false, nil
}
func (f *fakeMemory) TaskCreate(_ context.Context, _, _, _, subject, description string) (string, string, []sessionstore.Todo, error) {
	c := subject
	if description != "" {
		c = subject + ": " + description
	}
	f.created = append(f.created, c)
	id := "t" + strconv.Itoa(len(f.todos)+1)
	f.todos = append(f.todos, sessionstore.Todo{ID: id, Text: c, Status: "pending"})
	return id, c, f.todos, nil
}
func (f *fakeMemory) TaskUpdate(_ context.Context, _, _, _, taskID, status string) ([]sessionstore.Todo, error) {
	if f.updated == nil {
		f.updated = map[string]string{}
	}
	f.updated[taskID] = status
	for i := range f.todos {
		if f.todos[i].ID == taskID {
			f.todos[i].Status = status
		}
	}
	return f.todos, nil
}

func memCall(name string, args map[string]any) runtime.ToolInvocation {
	return runtime.ToolInvocation{
		Name: "memory." + name, Args: args,
		AppID: "app", AgentID: "main", SessionID: "sess", UserID: "u",
	}
}

func TestMemoryTools_Dispatch(t *testing.T) {
	f := &fakeMemory{}
	m := &meta.MetaDispatcher{Memory: f}

	if out := m.Dispatch(context.Background(), memCall("set_goal", map[string]any{"goal": "ship it"})); out.Status != "completed" {
		t.Fatalf("set_goal: %+v", out)
	}
	if f.goal != "ship it" {
		t.Errorf("goal not forwarded: %q", f.goal)
	}

	if out := m.Dispatch(context.Background(), memCall("remember", map[string]any{"content": "fact A"})); out.Status != "completed" {
		t.Fatalf("remember: %+v", out)
	}
	if len(f.remembered) != 1 || f.remembered[0] != "fact A" {
		t.Errorf("remember not forwarded: %+v", f.remembered)
	}

	tc := m.Dispatch(context.Background(), memCall("task_create", map[string]any{"subject": "do X", "description": "why"}))
	if tc.Status != "completed" || !strings.Contains(tc.Parts[0].Text, "t1") {
		t.Fatalf("task_create: %+v", tc)
	}
	if len(f.created) != 1 || f.created[0] != "do X: why" {
		t.Errorf("task_create content wrong: %+v", f.created)
	}

	if out := m.Dispatch(context.Background(), memCall("task_update", map[string]any{"task_id": "t1", "status": "in_progress"})); out.Status != "completed" {
		t.Fatalf("task_update: %+v", out)
	}
	if f.updated["t1"] != "in_progress" {
		t.Errorf("task_update not forwarded: %+v", f.updated)
	}
}

// task_create / task_update must hand the agent a "powerful context" in the
// result : progress, the single NEXT task, everything still open, and a nudge.
// When the plan finishes, it must say so (ties into not stopping early).
func TestTaskTools_ReturnRichPlanContext(t *testing.T) {
	f := &fakeMemory{}
	m := &meta.MetaDispatcher{Memory: f}
	ctx := context.Background()
	m.Dispatch(ctx, memCall("task_create", map[string]any{"subject": "design the schema"}))   // t1
	m.Dispatch(ctx, memCall("task_create", map[string]any{"subject": "write the migration"})) // t2
	m.Dispatch(ctx, memCall("task_create", map[string]any{"subject": "add a test"}))           // t3

	out := m.Dispatch(ctx, memCall("task_update", map[string]any{"task_id": "t1", "status": "in_progress"}))
	text := out.Parts[0].Text
	for _, want := range []string{"Plan: 0/3 done", "Next → t1", "Remaining", "t2", "t3", "don't end your turn"} {
		if !strings.Contains(text, want) {
			t.Errorf("plan context missing %q in:\n%s", want, text)
		}
	}

	// Finish everything → the context must signal the plan is done.
	m.Dispatch(ctx, memCall("task_update", map[string]any{"task_id": "t1", "status": "completed"}))
	m.Dispatch(ctx, memCall("task_update", map[string]any{"task_id": "t2", "status": "completed"}))
	done := m.Dispatch(ctx, memCall("task_update", map[string]any{"task_id": "t3", "status": "completed"}))
	if !strings.Contains(done.Parts[0].Text, "Every task is complete") {
		t.Errorf("all-done context should signal completion:\n%s", done.Parts[0].Text)
	}
}

func TestMemoryTools_DedupReported(t *testing.T) {
	f := &fakeMemory{dedupNext: true}
	m := &meta.MetaDispatcher{Memory: f}
	out := m.Dispatch(context.Background(), memCall("remember", map[string]any{"content": "dup"}))
	if out.Status != "completed" || !strings.Contains(out.Parts[0].Text, "already remembered") {
		t.Errorf("dedup should be reported: %+v", out)
	}
}

func TestMemoryTools_Validation(t *testing.T) {
	m := &meta.MetaDispatcher{Memory: &fakeMemory{}}
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"set_goal", map[string]any{}},
		{"remember", map[string]any{}},
		{"task_create", map[string]any{}},
		{"task_update", map[string]any{"task_id": "t1"}},
	} {
		if out := m.Dispatch(context.Background(), memCall(tc.name, tc.args)); out.Status != "errored" {
			t.Errorf("%s with missing args must error, got %+v", tc.name, out)
		}
	}
}

func TestMemoryTools_NotWired(t *testing.T) {
	m := &meta.MetaDispatcher{} // no Memory
	out := m.Dispatch(context.Background(), memCall("set_goal", map[string]any{"goal": "x"}))
	if out.Status != "errored" || !strings.Contains(out.Error, "not wired") {
		t.Errorf("expected not-wired error, got %+v", out)
	}
}
