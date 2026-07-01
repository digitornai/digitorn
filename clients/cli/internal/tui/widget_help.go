package tui

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/digitornai/digitorn-cli/internal/theme"
)

// renderHelp draws the help overlay : a centered card listing every
// command and keybinding. Dismissed by any key (handled by the caller).
func renderHelp(t *theme.Theme, width, height int) string {
	title := lipgloss.NewStyle().Foreground(lipgloss.Color(t.Primary)).Bold(true)
	head := lipgloss.NewStyle().Foreground(lipgloss.Color(t.Accent)).Bold(true)
	key := lipgloss.NewStyle().Foreground(lipgloss.Color(t.Secondary))
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color(t.TextMuted))

	row := func(k, desc string) string {
		return "  " + key.Render(padRight(k, 12)) + muted.Render(desc)
	}

	var b strings.Builder
	b.WriteString(title.Render("digitorn — help") + "\n\n")

	b.WriteString(head.Render("Commands") + muted.Render("  (type / or ctrl+k)") + "\n")
	for _, c := range allSlashCommands {
		b.WriteString(row("/"+c.Name, c.Desc) + "\n")
	}

	b.WriteString("\n" + head.Render("Keys") + "\n")
	for _, kv := range [][2]string{
		{"ctrl+k", "command palette"},
		{"ctrl+s / ctrl+a", "switch session / app"},
		{"ctrl+p / ctrl+n", "select previous / next message"},
		{"enter", "open the selected sub-agent (◇) · esc to go back"},
		{"ctrl+f", "search the transcript (↑↓ nav · esc close)"},
		{"ctrl+r", "retry the last message (recover a failed turn)"},
		{"ctrl+y", "copy selected message (or last reply)"},
		{"ctrl+o", "expand / collapse all tool results"},
		{"ctrl+u", "clear the input"},
		{"esc", "cancel · interrupt a running turn"},
		{"↑ / ↓", "previous / next sent message"},
		{"enter", "send   ·   shift+enter  newline"},
		{"ctrl+q", "quit"},
	} {
		b.WriteString(row(kv[0], kv[1]) + "\n")
	}

	b.WriteString("\n" + head.Render("Mouse") + "\n")
	b.WriteString(row("click ▸", "expand a collapsed tool result") + "\n")
	b.WriteString(row("shift+drag", "select / copy text (mouse is captured)") + "\n")
	b.WriteString(row("wheel", "scroll the transcript") + "\n")

	b.WriteString("\n" + muted.Render("press any key to close"))

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(t.BorderActive)).
		Background(lipgloss.Color(t.BackgroundPanel)).
		Foreground(lipgloss.Color(t.Text)).
		Padding(1, 3).
		Render(b.String())

	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box,
		lipgloss.WithWhitespaceChars(" "))
}

func padRight(s string, w int) string {
	if lipgloss.Width(s) >= w {
		return s + " "
	}
	return s + strings.Repeat(" ", w-lipgloss.Width(s))
}
