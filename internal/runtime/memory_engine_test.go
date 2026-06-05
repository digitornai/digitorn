package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/llm"
)

// TestEngineMemory_ToolsEventSourced proves the engine's MemoryWriter : every
// mutation lands as a durable session event projected into state (goal, facts,
// todos), facts dedup, secrets are redacted, task ids auto-increment, and an
// update to an unknown task errors.
func TestEngineMemory_ToolsEventSourced(t *testing.T) {
	apps := &stubApps{app: okApp(t, "", "", schema.Brain{Provider: "openai", Model: "gpt-4o-mini"})}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{resp: &llm.ChatResponse{Content: "ok"}}
	e := newEngine(t, apps, sess, lc)
	ctx := context.Background()

	if err := e.SetGoal(ctx, "sess-1", "app-1", "u", "Ship the memory feature"); err != nil {
		t.Fatalf("SetGoal: %v", err)
	}

	if _, dedup, err := e.Remember(ctx, "sess-1", "app-1", "u", "Test: go test ./..."); err != nil || dedup {
		t.Fatalf("first remember: dedup=%v err=%v", dedup, err)
	}
	if _, dedup, _ := e.Remember(ctx, "sess-1", "app-1", "u", "  TEST: go test ./...  "); !dedup {
		t.Error("case + surrounding-space variant must be reported as dedup")
	}
	// Secret must be redacted before it's persisted.
	if _, _, err := e.Remember(ctx, "sess-1", "app-1", "u", "deploy key api_key: SUPERSECRETVALUE123"); err != nil {
		t.Fatalf("remember secret: %v", err)
	}

	id1, _, todos1, err := e.TaskCreate(ctx, "sess-1", "app-1", "u", "Patch validate.go", "add null check")
	if err != nil || id1 != "t1" {
		t.Fatalf("task_create 1: id=%q err=%v", id1, err)
	}
	if len(todos1) != 1 {
		t.Errorf("task_create should return the plan (1 task), got %d", len(todos1))
	}
	id2, content2, _, _ := e.TaskCreate(ctx, "sess-1", "app-1", "u", "Add test", "")
	if id2 != "t2" || content2 != "Add test" {
		t.Errorf("task_create 2: id=%q content=%q", id2, content2)
	}
	todosU, err := e.TaskUpdate(ctx, "sess-1", "app-1", "u", "t1", "in_progress")
	if err != nil {
		t.Fatalf("task_update: %v", err)
	}
	if len(todosU) != 2 {
		t.Errorf("task_update should return the full plan (2 tasks), got %d", len(todosU))
	}
	if _, err := e.TaskUpdate(ctx, "sess-1", "app-1", "u", "t99", "done"); err == nil {
		t.Error("update of unknown task must error")
	}

	st, _ := sess.State("sess-1")
	snap := st.Snapshot()

	if snap.Goal != "Ship the memory feature" {
		t.Errorf("goal = %q", snap.Goal)
	}
	if len(snap.Facts) != 2 {
		t.Fatalf("facts = %d (want 2 : dup skipped), %v", len(snap.Facts), snap.Facts)
	}
	var sawRedacted, leaked bool
	for _, f := range snap.Facts {
		if strings.Contains(f, "[REDACTED]") {
			sawRedacted = true
		}
		if strings.Contains(f, "SUPERSECRETVALUE123") {
			leaked = true
		}
	}
	if !sawRedacted {
		t.Error("secret fact should be redacted")
	}
	if leaked {
		t.Error("SECRET LEAKED into stored fact")
	}
	if len(snap.Todos) != 2 || snap.Todos[0].ID != "t1" || snap.Todos[0].Status != "in_progress" {
		t.Errorf("todos wrong: %+v", snap.Todos)
	}
}
