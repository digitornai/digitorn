package tui

import (
	"fmt"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/mbathepaul/digitorn-cli/internal/theme"
)

// ApprovalPrompt is the modal shown when the daemon emits
// approval_request : a tool the agent wants to run is gated behind the
// capabilities `approve` policy and the turn is suspended until the
// user answers. y/a grant, n/d/esc deny.
type ApprovalPrompt struct {
	theme  *theme.Theme
	id     string
	tool   string
	risk   string
	reason string
	params map[string]any

	// submitting flips true once the user answered and the REST call
	// is in flight ; keys are ignored until the daemon's
	// approval_granted/denied envelope clears the prompt.
	submitting bool
	decision   string

	// bg is the modal's base style carrying the panel background — every inner
	// span derives from it so the elevated fill is solid across ANSI resets.
	bg lipgloss.Style
}

// NewApprovalPrompt builds a prompt from an approval_request payload
// (the wire shape of sessionstore.ApprovalPayload). Returns nil when
// the payload carries no id — there is nothing actionable to show.
func NewApprovalPrompt(t *theme.Theme, payload map[string]any) *ApprovalPrompt {
	id := mapStr(payload, "id")
	if id == "" {
		return nil
	}
	p := &ApprovalPrompt{
		theme:  t,
		id:     id,
		tool:   mapStr(payload, "tool_name"),
		risk:   mapStr(payload, "risk_level"),
		reason: mapStr(payload, "reason"),
	}
	if tp, ok := payload["tool_params"].(map[string]any); ok {
		p.params = tp
	}
	return p
}

func (p *ApprovalPrompt) ID() string { return p.id }

// Card renders the approval as a bordered panel sized to fit within
// `width` columns, meant to be docked at the bottom of the chat (in
// place of the composer) rather than as a full-screen overlay — so the
// transcript stays visible above it.
func (p *ApprovalPrompt) Card(width int) string {
	innerW := width - 7 // -6 box chrome, -1 reserved for the drop-shadow column
	if innerW < 28 {
		innerW = 28
	}
	if innerW > 96 {
		innerW = 96
	}

	// Elevated panel : filled background + a Warning-coloured rounded border. Every
	// inner span derives from p.bg so the fill is solid across ANSI resets.
	panel := lipgloss.Color(p.theme.BackgroundElement)
	p.bg = lipgloss.NewStyle().Background(panel)
	muted := p.bg.Foreground(lipgloss.Color(p.theme.TextMuted))

	title := p.bg.Foreground(lipgloss.Color(p.theme.Warning)).Bold(true).Render("⚠") +
		muted.Bold(true).Render("  APPROVAL REQUIRED")
	divider := p.bg.Foreground(lipgloss.Color(p.theme.BorderSubtle)).Render(strings.Repeat("─", innerW))

	rows := []string{title, "", divider, ""}
	rows = append(rows, p.field("Tool", orDash(p.tool), innerW))
	if p.risk != "" {
		rows = append(rows, p.field("Risk", p.riskBadge(), innerW))
	}
	if p.reason != "" {
		rows = append(rows, p.field("Reason", p.reason, innerW))
	}
	if params := p.renderParams(innerW); params != "" {
		rows = append(rows, "", muted.Render("Params"), params)
	}

	var footer string
	if p.submitting {
		footer = muted.Render(fmt.Sprintf("submitting %s …", p.decision))
	} else {
		approve := p.bg.Foreground(lipgloss.Color(p.theme.Success)).Bold(true).Render("[y/a] approve")
		deny := p.bg.Foreground(lipgloss.Color(p.theme.Error)).Bold(true).Render("[n/d/esc] deny")
		footer = approve + muted.Render("   ") + deny
	}
	rows = append(rows, "", footer)

	// Fill every line to the inner width so the background paints edge-to-edge.
	for i := range rows {
		rows[i] = padLinesBg(rows[i], innerW, p.bg)
	}

	content := lipgloss.JoinVertical(lipgloss.Left, rows...)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(p.theme.Warning)).
		BorderBackground(lipgloss.Color(p.theme.Background)).
		Background(panel).
		Padding(1, 2).
		Width(innerW).
		Render(content)
	return dropShadow(box, p.theme)
}

func (p *ApprovalPrompt) field(label, value string, width int) string {
	lab := p.bg.
		Foreground(lipgloss.Color(p.theme.TextMuted)).
		Width(8).
		Render(label)
	val := p.bg.
		Foreground(lipgloss.Color(p.theme.Text)).
		Width(width - 10).
		Render(value)
	return lipgloss.JoinHorizontal(lipgloss.Top, lab, val)
}

func (p *ApprovalPrompt) riskBadge() string {
	color := p.theme.Info
	switch strings.ToLower(p.risk) {
	case "high":
		color = p.theme.Error
	case "medium":
		color = p.theme.Warning
	case "low":
		color = p.theme.Success
	}
	return p.bg.Foreground(lipgloss.Color(color)).Bold(true).Render(p.risk)
}

// renderParams shows the tool arguments as sorted key: value lines,
// each value truncated to keep the modal compact. Empty when the call
// carries no params.
func (p *ApprovalPrompt) renderParams(width int) string {
	if len(p.params) == 0 {
		return ""
	}
	keys := make([]string, 0, len(p.params))
	for k := range p.params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var lines []string
	for _, k := range keys {
		val := fmt.Sprintf("%v", p.params[k])
		val = strings.ReplaceAll(val, "\n", " ")
		max := width - len(k) - 6
		if max < 8 {
			max = 8
		}
		if len(val) > max {
			val = val[:max-1] + "…"
		}
		line := p.bg.Foreground(lipgloss.Color(p.theme.Accent)).Render("  "+k+": ") +
			p.bg.Foreground(lipgloss.Color(p.theme.Text)).Render(val)
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func mapStr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
