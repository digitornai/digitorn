package tui

import (
	"fmt"
	"os"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/mbathepaul/digitorn-cli/internal/theme"
)

// Sidebar is the right rail. CLI-3 = char counter + key hints. CLI-4
// adds the live turn timeline (most recent N events). Later sprints
// stack more sections vertically.
type Sidebar struct {
	theme  *theme.Theme
	width  int
	height int
}

type SidebarStats struct {
	// App context : rendered as a bold header at the very top of the
	// sidebar. Replaces the `app:NAME` segment we used to show in the
	// statusbar — bottom-bar real estate is precious, the sidebar has
	// room to give the app a prominent slot.
	AppName string

	// Workdir is the session's working directory on the daemon host,
	// shown muted under the app name (opencode surfaces the cwd up top).
	Workdir string

	// Turn lifecycle — fed from realtime envelopes. Drives the "[running]"
	// badge ; the rail no longer lists started/phase/ended rows.
	Phase       string
	PendingTurn bool

	// Todos is the agent's task list, always visible here so the user can
	// follow the plan as the loop advances (mirrors the inline chat block).
	Todos []todoItem

	// SpinFrame is the shared animation frame so the activity panel's running
	// tool rows show the same live spinner as the chat chips.
	SpinFrame int

	// Approval is the label of the approval/question awaiting the user now
	// ("" if none) — shown as a single live line, not an approved/denied log.
	Approval string

	// SubAgents are the active sub-agent activity groups : each renders a
	// PINNED header + a bounded window of its recent tools, dropped when the
	// sub-agent finishes. Keeps the header visible while tools scroll under it.
	SubAgents []SubAgentView

	// Mode is the active composer mode id (runtime.modes), shown in parentheses
	// after the app name in the footer ("Chat Context Demo (summarize)"). Empty
	// when the app declares no modes.
	Mode string

	// Version is the app's version, shown right after the name in the footer
	// ("Chat Context Demo v0.1.0"). Empty until fetched / when unknown.
	Version string
}

// SubAgentView is one sub-agent's activity slice for the panel.
type SubAgentView struct {
	Kind     string
	Finished []TimelineEntry // recent ✓/✗ rows — feeds the "· N tools" count only
	Running  []TimelineEntry // in-flight rows (shown up to a cap, then "⠋ … xN")
	Settling []TimelineEntry // just-finished ghosts fading out
	Hidden   int             // finished rows scrolled off (top "…")
}

func NewSidebar(t *theme.Theme) *Sidebar {
	return &Sidebar{theme: t}
}

func (s *Sidebar) SetSize(w, h int) {
	if w < 10 {
		w = 10
	}
	if h < 3 {
		h = 3
	}
	s.width = w
	s.height = h
}

func (s *Sidebar) Width() int { return s.width }

func (s *Sidebar) View(stats SidebarStats) string {
	if s.width == 0 || s.height == 0 {
		return ""
	}
	contentW := s.width - 3
	if contentW < 8 {
		contentW = 8
	}

	// Layout (top→bottom) : everything FLOWS from the top — TURN status, then the
	// TASKS plan, then the live sub-agent ACTIVITY right under it (it does NOT
	// jump to the footer ; it sits directly below the previous block and grows
	// down into the gap). ONLY the app identity footer is pinned to the bottom.
	var top []string
	top = append(top, s.renderTurnStatus(stats, contentW))
	if td := s.renderTodoSection(stats, contentW); td != "" {
		top = append(top, "", td)
	}
	if act := s.renderActivity(stats, contentW); act != "" {
		top = append(top, "", act)
	}

	footer := s.renderAppFooter(stats, contentW)
	lines := strings.Split(lipgloss.JoinVertical(lipgloss.Left, top...), "\n")
	target := s.height - lipgloss.Height(footer)
	for len(lines) < target {
		lines = append(lines, "")
	}
	lines = append(lines, strings.Split(footer, "\n")...)
	body := strings.Join(lines, "\n")

	return lipgloss.NewStyle().
		Width(s.width).
		Height(s.height).
		MaxHeight(s.height).
		BorderStyle(lipgloss.ThickBorder()).
		BorderLeft(true).
		BorderForeground(lipgloss.Color(s.theme.BorderSubtle)).
		Padding(0, 1).
		Render(body)
}

func (s *Sidebar) renderAppFooter(stats SidebarStats, contentW int) string {
	name := stats.AppName
	if name == "" {
		name = "(no app)"
	}
	if stats.Version != "" {
		v := stats.Version
		if !strings.HasPrefix(v, "v") {
			v = "v" + v
		}
		name += " " + v
	}
	if stats.Mode != "" {
		name += " (" + stats.Mode + ")"
	}
	title := lipgloss.NewStyle().
		Foreground(lipgloss.Color(s.theme.Primary)).
		Bold(true).
		Render(truncate(name, contentW))
	rows := []string{title}
	if wd := collapseHome(stats.Workdir); wd != "" {
		rows = append(rows, lipgloss.NewStyle().
			Foreground(lipgloss.Color(s.theme.TextMuted)).
			Render(truncate(wd, contentW)))
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// collapseHome shortens an absolute path under $HOME to "~/…", matching
// opencode's cwd display. Returns the input unchanged when it doesn't
// live under home.
func collapseHome(path string) string {
	if path == "" {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(path, home) {
		return "~" + path[len(home):]
	}
	return path
}

// sectionHeader renders a section header as a coloured accent bar + the label in
// muted UPPERCASE — "▌ TASKS" — with an optional pre-styled suffix pinned to the
// rail's right edge (the turn badge). No dividing rule : the bar marks the
// section, the caps carry the label.
func (s *Sidebar) sectionHeader(label, right string, contentW int) string {
	bar := lipgloss.NewStyle().Foreground(lipgloss.Color(s.theme.Primary)).Bold(true).Render("▌ ")
	lbl := lipgloss.NewStyle().Foreground(lipgloss.Color(s.theme.TextMuted)).Bold(true).
		Render(strings.ToUpper(label))
	left := bar + lbl
	if right == "" {
		return left
	}
	gap := contentW - lipgloss.Width("▌ ") - lipgloss.Width(label) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// renderTurnStatus is the top-of-rail glance : the "▌ TURN [running|idle]"
// header plus any approval/question awaiting the user right now. Always present
// (the badge gives the live turn state) ; the sub-agent activity lives at the
// bottom of the rail instead (renderActivity).
func (s *Sidebar) renderTurnStatus(stats SidebarStats, contentW int) string {
	// Status badge, always present for consistency : "[running]"/"[<phase>]"
	// while a turn is in flight, "[idle]" (muted) when nothing's running. Pinned
	// to the right of the "turn" rule.
	phase, phaseColor := "idle", s.theme.TextMuted
	if stats.PendingTurn {
		phase, phaseColor = stats.Phase, s.phaseColor(stats.Phase)
		if phase == "" {
			phase, phaseColor = "running", s.phaseColor("running")
		}
	}
	badge := lipgloss.NewStyle().Foreground(lipgloss.Color(phaseColor)).Bold(true).Render("[" + phase + "]")

	lines := []string{s.sectionHeader("turn", badge, contentW)}
	// The single approval/question awaiting the user right now (if any).
	if stats.Approval != "" {
		lines = append(lines, "", lipgloss.NewStyle().Foreground(lipgloss.Color(s.theme.Warning)).Bold(true).
			Render(truncate("⏸ "+stats.Approval, contentW)))
	}
	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}

// renderActivity is the bottom-of-rail live view of sub-agent work. Empty when
// no sub-agent is running. The MAIN agent's tools are NOT shown here — they're
// already chips in the chat ; the rail is reserved for sub-agent work, which
// the chat doesn't surface inline. Only in-flight tools, with a brief fade-out
// ghost. Each active sub-agent : a header "◇ Agent <kind> · N tools" (kind in
// the agent's colour, "Agent" + count muted), then ONLY its in-flight tools —
// the "· N tools" count carries the history. The group vanishes when it ends.
func (s *Sidebar) renderActivity(stats SidebarStats, contentW int) string {
	if len(stats.SubAgents) == 0 {
		return ""
	}
	faint := lipgloss.NewStyle().Foreground(lipgloss.Color(s.theme.TextMuted)).Faint(true)
	// Own section header so the live sub-agent work reads as a distinct block,
	// not as noise glued to the app footer below it.
	lines := []string{s.sectionHeader("activity", "", contentW), ""}
	for _, sa := range stats.SubAgents {
		cStr := agentColorFor(sa.Kind, s.theme)
		agentStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(cStr)).Bold(true)
		labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(s.theme.Secondary))
		// Animated spinner (not a static ◇) so it's obvious the sub-agent is still
		// alive even when it's just generating its reply with no tool running — the
		// group only exists while the sub-agent runs, so this always animates.
		// Reserve room for the spinner + " Agent " (8) and the " · NN tools"
		// suffix (~12) so a long kind can't push the header past the rail edge.
		header := agentStyle.Render(spinnerGlyph(stats.SpinFrame)) +
			labelStyle.Render(" Agent ") +
			agentStyle.Render(truncate(sa.Kind, contentW-20))
		if n := sa.Hidden + len(sa.Finished) + len(sa.Running); n > 0 { // true total
			header += faint.Render(fmt.Sprintf(" · %d tools", n))
		}
		lines = append(lines, header)
		for _, tl := range sa.Settling {
			lines = append(lines, s.settledRow("  ╰ ", tl, contentW-4))
		}
		lines = s.appendRunning(lines, sa.Running, cStr, "  ╰ ", stats.SpinFrame, contentW)
	}

	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}

// renderTodoSection draws the agent's task list — always visible so the user
// can follow the plan as the loop advances. Empty (skipped) when no tasks.
func (s *Sidebar) renderTodoSection(stats SidebarStats, contentW int) string {
	if len(stats.Todos) == 0 {
		return ""
	}
	// Cap the rail at sidebarTodoMax, prioritising unfinished work over finished
	// tasks (a long completed plan must not bury the live ones). The header badge
	// shows overall progress ; a footer notes how many are hidden.
	shown, done, hidden := pickSidebarTodos(stats.Todos, sidebarTodoMax)
	badge := fmt.Sprintf("%d/%d", done, len(stats.Todos))
	lines := []string{s.sectionHeader("tasks", badge, contentW), ""}
	for _, td := range shown {
		g, c := todoGlyph(td.Status, s.theme)
		glyph := lipgloss.NewStyle().Foreground(lipgloss.Color(c)).Render(g)
		txt := lipgloss.NewStyle().Foreground(lipgloss.Color(s.theme.Text))
		switch td.Status {
		case "completed", "done", "ok", "success":
			txt = txt.Faint(true).Strikethrough(true)
		case "blocked", "errored", "error", "failed", "cancelled":
			txt = lipgloss.NewStyle().Foreground(lipgloss.Color(s.theme.Error))
		case "in_progress", "running", "active":
			txt = txt.Bold(true)
		default:
			txt = txt.Faint(true)
		}
		lines = append(lines, glyph+" "+txt.Render(truncate(td.Text, contentW-2)))
	}
	if hidden > 0 {
		more := lipgloss.NewStyle().Foreground(lipgloss.Color(s.theme.TextMuted)).Faint(true)
		lines = append(lines, more.Render(fmt.Sprintf("  +%d more", hidden)))
	}
	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}

// appendRunning lists in-flight tools individually (each with the live spinner)
// up to subActivityRunMax, then collapses any overflow into one "⠋ … xN" line —
// so some current work is always visible without a parallel burst flooding the
// rail. prefix is "" for the main agent, "  ╰ " for a sub-agent.
func (s *Sidebar) appendRunning(lines []string, running []TimelineEntry, spinColor, prefix string, spin, contentW int) []string {
	conn := lipgloss.NewStyle().Foreground(lipgloss.Color(s.theme.TextMuted)).Faint(true)
	spinStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(spinColor))
	txt := lipgloss.NewStyle().Foreground(lipgloss.Color(s.theme.Text))
	w := contentW - len([]rune(prefix)) - 2
	// When more than the cap are in flight, collapse the OLDER ones into a single
	// "⠋ … xN" line first, then list the most recent ones individually below.
	start := 0
	if len(running) > subActivityRunMax {
		start = len(running) - subActivityRunMax
		lines = append(lines, conn.Render(prefix)+spinStyle.Render(spinnerGlyph(spin))+conn.Render(fmt.Sprintf(" … x%d", start)))
	}
	for i := start; i < len(running); i++ {
		rowTxt := txt
		if running[i].age < activityFadeTicks { // first tick : fade in, don't pop
			rowTxt = conn
		}
		lines = append(lines, conn.Render(prefix)+spinStyle.Render(spinnerGlyph(spin))+rowTxt.Render(" "+truncate(running[i].Label, w)))
	}
	return lines
}

// settledRow renders a just-finished tool as a faint ✓/✗ ghost — the brief
// fade-out the row gets before it's pruned from the rail.
func (s *Sidebar) settledRow(prefix string, e TimelineEntry, w int) string {
	glyph, gc := "✓", s.theme.Success
	if !e.ok {
		glyph, gc = "✗", s.theme.Error
	}
	conn := lipgloss.NewStyle().Foreground(lipgloss.Color(s.theme.TextMuted)).Faint(true)
	g := lipgloss.NewStyle().Foreground(lipgloss.Color(gc)).Faint(true)
	return conn.Render(prefix) + g.Render(glyph) + conn.Render(" "+truncate(e.Label, w-2))
}

func (s *Sidebar) phaseColor(phase string) string {
	switch phase {
	case "thinking", "running":
		return s.theme.PhaseThinking
	case "tool_use", "dispatching":
		return s.theme.PhaseToolUse
	case "persisting", "loading":
		return s.theme.PhasePersisting
	case "done":
		return s.theme.PhaseDone
	}
	return s.theme.TextMuted
}

// truncate clips s to at most max display cells, ending in "…". Rune-based so
// it never cuts a multibyte character in half (which would garble accents).
func truncate(s string, max int) string {
	if max <= 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
