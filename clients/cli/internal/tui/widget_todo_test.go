package tui

import (
	"strconv"
	"testing"
)

func ids(todos []todoItem) string {
	out := ""
	for i, td := range todos {
		if i > 0 {
			out += ","
		}
		out += td.ID
	}
	return out
}

// The reported case : a long plan, 13 done + 1 in_progress. The rail must show
// the in_progress task FIRST, then the most recent completed to fill 6 slots,
// report 13/14 done, and hide the rest.
func TestPickSidebarTodos_PrioritisesUnfinished(t *testing.T) {
	var todos []todoItem
	for i := 1; i <= 13; i++ {
		todos = append(todos, todoItem{ID: strconv.Itoa(i), Status: "completed"})
	}
	todos = append(todos, todoItem{ID: "14", Status: "in_progress"})

	shown, done, hidden := pickSidebarTodos(todos, sidebarTodoMax)
	if len(shown) != 6 {
		t.Fatalf("want 6 shown, got %d (%s)", len(shown), ids(shown))
	}
	if shown[0].ID != "14" {
		t.Fatalf("in_progress must come first, got %q", shown[0].ID)
	}
	// Remaining 5 are the most recent completed (t9..t13), in order.
	if got := ids(shown[1:]); got != "9,10,11,12,13" {
		t.Fatalf("fill must be the 5 most recent completed, got %q", got)
	}
	if done != 13 {
		t.Fatalf("done count = %d, want 13", done)
	}
	if hidden != 8 {
		t.Fatalf("hidden = %d, want 8", hidden)
	}
}

// When there are more unfinished tasks than the cap, only unfinished show
// (in_progress before pending), and no completed leak in.
func TestPickSidebarTodos_AllSlotsToUnfinished(t *testing.T) {
	todos := []todoItem{
		{ID: "1", Status: "completed"},
		{ID: "2", Status: "pending"},
		{ID: "3", Status: "pending"},
		{ID: "4", Status: "in_progress"},
		{ID: "5", Status: "pending"},
		{ID: "6", Status: "pending"},
		{ID: "7", Status: "pending"},
		{ID: "8", Status: "pending"},
	}
	shown, done, hidden := pickSidebarTodos(todos, sidebarTodoMax)
	if len(shown) != 6 {
		t.Fatalf("want 6 shown, got %d", len(shown))
	}
	if shown[0].ID != "4" {
		t.Fatalf("in_progress must lead, got %q", shown[0].ID)
	}
	// No completed task should appear while unfinished work fills the cap.
	for _, td := range shown {
		if todoDone(td.Status) {
			t.Fatalf("completed task %q leaked in despite unfinished overflow", td.ID)
		}
	}
	if done != 1 || hidden != 2 {
		t.Fatalf("done=%d hidden=%d, want 1 and 2", done, hidden)
	}
}

func TestPickSidebarTodos_ShortListUnchanged(t *testing.T) {
	todos := []todoItem{
		{ID: "1", Status: "completed"},
		{ID: "2", Status: "in_progress"},
	}
	shown, done, hidden := pickSidebarTodos(todos, sidebarTodoMax)
	if len(shown) != 2 || hidden != 0 || done != 1 {
		t.Fatalf("short list: shown=%d done=%d hidden=%d", len(shown), done, hidden)
	}
	if shown[0].ID != "2" {
		t.Fatalf("in_progress must lead even in a short list, got %q", shown[0].ID)
	}
}
