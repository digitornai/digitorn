package prompt

import (
	"fmt"
	"strings"
	"testing"
)

func TestRenderWorkingMemory_Empty(t *testing.T) {
	if got := RenderWorkingMemory(WorkingMemoryView{}); got != "" {
		t.Errorf("empty working memory must render nothing, got %q", got)
	}
}

func TestRenderWorkingMemory_GoalTasksFacts(t *testing.T) {
	wm := WorkingMemoryView{
		Goal: "Fix the auth bug",
		Todos: []TodoLine{
			{ID: "t1", Text: "Reproduce", Status: "done"},
			{ID: "t2", Text: "Patch validate.go", Status: "in_progress"},
			{ID: "t3", Text: "Add test", Status: "pending"},
		},
		Facts: []string{"Bug is in auth/validate.go:42", "Test: go test ./auth/"},
	}
	got := RenderWorkingMemory(wm)

	for _, want := range []string{
		"Goal: Fix the auth bug",
		"Tasks (1/3 done):",
		"[x] [t1] Reproduce",
		"[~] [t2] Patch validate.go",
		"[ ] [t3] Add test  <- next",
		"Key facts:",
		"- Bug is in auth/validate.go:42",
		"- Test: go test ./auth/",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered snapshot missing %q\n--- got ---\n%s", want, got)
		}
	}
}

func TestRenderWorkingMemory_FactsCapped(t *testing.T) {
	facts := make([]string, 30)
	for i := range facts {
		// Zero-padded + unique-tail so no fact is a substring of another.
		facts[i] = fmt.Sprintf("fact-%03d-unique-%d", i, i*7+1)
	}
	got := RenderWorkingMemory(WorkingMemoryView{Facts: facts})
	// Only the most recent memoryFactsShown facts are rendered.
	if n := strings.Count(got, "\n  - "); n != memoryFactsShown {
		t.Errorf("rendered %d facts, want %d (capped)", n, memoryFactsShown)
	}
	// The newest fact (last in slice) must be present ; the oldest must not.
	if !strings.Contains(got, facts[len(facts)-1]) {
		t.Error("newest fact should be shown")
	}
	if strings.Contains(got, facts[0]) {
		t.Error("oldest fact should have been dropped by the display cap")
	}
}
