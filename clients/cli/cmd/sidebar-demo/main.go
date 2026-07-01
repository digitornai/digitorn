// Command sidebar-demo renders the activity-rail with each candidate section-
// header style side by side, using the real theme + border, so a style can be
// judged by eye in the terminal rather than from an ASCII mockup.
package main

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/digitornai/digitorn-cli/internal/theme"
)

const railW = 30

func main() {
	t := theme.Default()

	styles := []struct {
		name   string
		header func(label, right string) string
	}{
		{"barre d'accent ▌", accentBar(t)},
		{"MAJUSCULES atténuées", upper(t)},
		{"accent diamant ◈", diamond(t)},
		{"actuel (filet intégré)", rule(t)},
	}

	var boxes []string
	for _, st := range styles {
		title := lipgloss.NewStyle().Foreground(lipgloss.Color(t.Accent)).Bold(true).
			Width(railW).Align(lipgloss.Center).Render(st.name)
		boxes = append(boxes, lipgloss.JoinVertical(lipgloss.Left, title, "", rail(t, st.header)))
	}
	fmt.Println()
	fmt.Println(lipgloss.JoinHorizontal(lipgloss.Top, spread(boxes, "   ")...))
	fmt.Println()
}

func spread(items []string, gap string) []string {
	out := make([]string, 0, len(items)*2)
	for i, it := range items {
		if i > 0 {
			out = append(out, gap)
		}
		out = append(out, it)
	}
	return out
}

// rail renders one activity rail using the given section-header style.
func rail(t *theme.Theme, header func(label, right string) string) string {
	pri := lipgloss.NewStyle().Foreground(lipgloss.Color(t.Primary)).Bold(true)
	mut := lipgloss.NewStyle().Foreground(lipgloss.Color(t.TextMuted))
	txt := lipgloss.NewStyle().Foreground(lipgloss.Color(t.Text))
	faint := mut.Faint(true)
	ok := lipgloss.NewStyle().Foreground(lipgloss.Color(t.Success))
	idle := lipgloss.NewStyle().Foreground(lipgloss.Color(t.TextMuted)).Bold(true).Render("[idle]")
	agent := lipgloss.NewStyle().Foreground(lipgloss.Color(t.Accent)).Bold(true)

	lines := []string{
		pri.Render("digitorn-cli"),
		mut.Render("~/proj/digitorn"),
		"",
		header("tasks", ""),
		ok.Render(" ✓ ") + txt.Faint(true).Strikethrough(true).Render("parse config"),
		lipgloss.NewStyle().Foreground(lipgloss.Color(t.Primary)).Render(" ▸ ") + txt.Bold(true).Render("wire engine"),
		mut.Render(" ○ ") + txt.Faint(true).Render("write tests"),
		"",
		header("turn", idle),
		agent.Render("◇") + lipgloss.NewStyle().Foreground(lipgloss.Color(t.Secondary)).Render(" Agent ") + agent.Render("explore") + faint.Render(" · 3 tools"),
		faint.Render("  ╰ ") + lipgloss.NewStyle().Foreground(lipgloss.Color(t.Primary)).Render("⠋") + txt.Render(" grep \"foo\""),
	}
	body := lipgloss.JoinVertical(lipgloss.Left, lines...)
	return lipgloss.NewStyle().
		Width(railW).
		BorderStyle(lipgloss.ThickBorder()).
		BorderLeft(true).
		BorderForeground(lipgloss.Color(t.BorderSubtle)).
		Padding(0, 1).
		Render(body)
}

// --- the candidate header styles -------------------------------------------

func accentBar(t *theme.Theme) func(string, string) string {
	bar := lipgloss.NewStyle().Foreground(lipgloss.Color(t.Primary)).Bold(true)
	lbl := lipgloss.NewStyle().Foreground(lipgloss.Color(t.Text)).Bold(true)
	return func(label, right string) string {
		h := bar.Render("▌ ") + lbl.Render(label)
		return padRight(t, h, right)
	}
}

func upper(t *theme.Theme) func(string, string) string {
	lbl := lipgloss.NewStyle().Foreground(lipgloss.Color(t.TextMuted)).Bold(true)
	return func(label, right string) string {
		h := lbl.Render(strings.ToUpper(label))
		return padRight(t, h, right)
	}
}

func diamond(t *theme.Theme) func(string, string) string {
	dia := lipgloss.NewStyle().Foreground(lipgloss.Color(t.Primary)).Bold(true)
	lbl := lipgloss.NewStyle().Foreground(lipgloss.Color(t.Text)).Bold(true)
	return func(label, right string) string {
		h := dia.Render("◈ ") + lbl.Render(label)
		return padRight(t, h, right)
	}
}

func rule(t *theme.Theme) func(string, string) string {
	faint := lipgloss.NewStyle().Foreground(lipgloss.Color(t.TextMuted)).Faint(true)
	lbl := lipgloss.NewStyle().Foreground(lipgloss.Color(t.Primary)).Bold(true)
	cw := railW - 3
	return func(label, right string) string {
		fill := cw - 4 - lipgloss.Width(label)
		if right != "" {
			fill -= lipgloss.Width(right) + 1
		}
		if fill < 1 {
			fill = 1
		}
		h := faint.Render("── ") + lbl.Render(label) + faint.Render(" "+strings.Repeat("─", fill))
		if right != "" {
			h += " " + right
		}
		return h
	}
}

// padRight pins `right` to the rail's right edge on the header line.
func padRight(t *theme.Theme, left, right string) string {
	if right == "" {
		return left
	}
	cw := railW - 3
	gap := cw - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}
