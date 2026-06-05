package tui

import (
	"sort"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/mbathepaul/digitorn-cli/internal/theme"
)

// sidebarTodoMax caps how many tasks the right rail shows at once : a long plan
// must not flood the rail with finished items. The unfinished work (in_progress
// / not-yet-started) takes priority ; finished tasks only fill leftover slots.
const sidebarTodoMax = 6

// todoDone reports whether a status counts as finished (won't be prioritised).
func todoDone(status string) bool {
	switch status {
	case "completed", "done", "ok", "success":
		return true
	}
	return false
}

// todoBucket ranks a status for display priority : current work first, then
// not-yet-started, then blocked, then finished. Lower sorts first.
func todoBucket(status string) int {
	switch status {
	case "in_progress", "running", "active":
		return 0
	case "blocked", "errored", "error", "failed", "cancelled":
		return 2
	case "completed", "done", "ok", "success":
		return 3
	default: // pending / ""
		return 1
	}
}

// pickSidebarTodos selects which tasks the sidebar shows, capped at max :
// unfinished first (in_progress → pending → blocked, keeping plan order within
// each bucket), then the MOST RECENT completed tasks fill any leftover slots so
// finished work is minimised but recent progress stays visible. Returns the
// chosen items, the total completed count (for the header badge), and how many
// tasks are hidden (for a "+N" footer).
func pickSidebarTodos(todos []todoItem, max int) (shown []todoItem, doneCount, hidden int) {
	if max <= 0 {
		return nil, 0, len(todos)
	}
	active := make([]todoItem, 0, len(todos))
	done := make([]todoItem, 0, len(todos))
	for _, td := range todos {
		if todoDone(td.Status) {
			done = append(done, td)
		} else {
			active = append(active, td)
		}
	}
	doneCount = len(done)
	sort.SliceStable(active, func(i, j int) bool {
		return todoBucket(active[i].Status) < todoBucket(active[j].Status)
	})
	shown = active
	if len(shown) > max {
		shown = shown[:max]
	} else if fill := max - len(shown); fill > 0 && len(done) > 0 {
		tail := done
		if len(tail) > fill {
			tail = tail[len(tail)-fill:]
		}
		shown = append(shown, tail...)
	}
	hidden = len(todos) - len(shown)
	return shown, doneCount, hidden
}

// todoItem is one task in the agent's plan, fed by the daemon's todo_added /
// todo_updated events (memory.task_create / task_update). The CLI keeps the
// ordered list and renders it as a checklist — inline in the chat and, always
// visible, in the right sidebar — instead of as raw tool chips.
type todoItem struct {
	ID     string
	Text   string
	Status string
}

// todoGlyph maps a task status to its checklist glyph + colour.
func todoGlyph(status string, t *theme.Theme) (glyph, color string) {
	switch status {
	case "in_progress", "running", "active":
		return "◔", t.Accent
	case "completed", "done", "ok", "success":
		return "☑", t.Success
	case "blocked", "errored", "error", "failed", "cancelled":
		return "☒", t.Error
	default: // pending / ""
		return "☐", t.TextMuted
	}
}

// renderTodoLines renders the task list as styled lines on the chip panel
// background (so the inline block's tint paints cleanly behind the text). The
// sidebar renders its own compact form. completed tasks read struck-through and
// faint ; pending ones faint ; the active one in normal weight. width is the
// chip's inner width : each task text is truncated to it with "…" so a long
// task can't overflow the block (the full text lives in the daemon's task).
func renderTodoLines(todos []todoItem, t *theme.Theme, width int) string {
	base := lipgloss.NewStyle().Background(lipgloss.Color(t.BackgroundPanel))
	var b strings.Builder
	for i, td := range todos {
		if i > 0 {
			b.WriteByte('\n')
		}
		g, c := todoGlyph(td.Status, t)
		glyph := base.Foreground(lipgloss.Color(c)).Render(g)
		txt := base.Foreground(lipgloss.Color(t.Text))
		switch td.Status {
		case "completed", "done", "ok", "success":
			txt = txt.Faint(true).Strikethrough(true)
		case "in_progress", "running", "active":
			txt = txt.Bold(true)
		case "blocked", "errored", "error", "failed", "cancelled":
			txt = base.Foreground(lipgloss.Color(t.Error))
		default:
			txt = txt.Faint(true)
		}
		label := td.Text
		if width > 2 && lipgloss.Width(label) > width-2 { // 2 = glyph + space
			label = truncate(label, width-2)
		}
		b.WriteString(glyph + base.Render(" ") + txt.Render(label))
	}
	return b.String()
}
