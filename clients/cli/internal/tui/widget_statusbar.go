package tui

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/digitornai/digitorn-cli/internal/theme"
)

// ConnState is the current connection status to the daemon. Drawn as
// a coloured dot at the left of the statusbar.
type ConnState int

const (
	ConnConnecting ConnState = iota
	ConnConnected
	ConnReconnecting
	ConnDisconnected
)

// String returns the human-readable variant.
func (c ConnState) String() string {
	switch c {
	case ConnConnecting:
		return "connecting"
	case ConnConnected:
		return "connected"
	case ConnReconnecting:
		return "reconnecting"
	case ConnDisconnected:
		return "disconnected"
	}
	return "?"
}

// StatusBar is the bottom-of-screen single-line indicator. Holds
// just enough state to re-render itself ; the parent Model is the
// authority on what to display.
type StatusBar struct {
	theme     *theme.Theme
	width     int
	bodyWidth int // chat body (input) width = terminal − sidebar

	AppName   string
	Model     string
	Session   string // short form (last 8 chars typically)
	Conn      ConnState
	UserEmail string
}

// NewStatusBar builds a statusbar bound to a theme. Width is set
// via SetWidth() on every WindowSizeMsg.
func NewStatusBar(t *theme.Theme) *StatusBar {
	return &StatusBar{theme: t, Conn: ConnConnecting}
}

// SetWidth must be called whenever the terminal resizes. It also derives
// the chat body's width (terminal minus sidebar), so the model segment
// can be aligned to the input block's right edge.
func (s *StatusBar) SetWidth(w int) {
	if w < 20 {
		w = 20
	}
	s.width = w
	bw := w - computeSidebarWidth(w) - bodySidebarGap
	if bw < 10 {
		bw = 10
	}
	s.bodyWidth = bw
}

// View renders one line of ANSI in three zones aligned to the chat layout
// above it :
//
//	●                       … model MODEL │ session ABCDE12
//	└ conn (far left)          └ ends at the input block's right edge
//	                                        └ session, far right (under sidebar)
//
// The model is pinned to bodyWidth so it lines up with the end of the
// composer ; the session sits past it, in the sidebar column.
func (s *StatusBar) View() string {
	if s.width == 0 {
		s.width = 80
	}
	bw := s.bodyWidth
	if bw <= 0 || bw > s.width {
		bw = s.width
	}
	conn := s.renderConnDot()
	model := s.modelSegment()

	// Measure on PLAIN text : lipgloss.Width undercounts a string built
	// from several concatenated styled spans, which would misplace the
	// model by a couple of columns. Styling never changes display width,
	// so the plain measurement is the truthful one.
	gapA := bw - lipgloss.Width(conn) - plainWidth(s.Model, "model ")
	if gapA < 1 {
		gapA = 1
	}
	// Continue the sidebar's left border down through the status bar so the
	// chat↔rail divider reaches the very bottom of the screen instead of stopping
	// at the body. The bar sits at the same column as the rail's border above
	// (chat width + the body↔rail gap), in the same BorderSubtle colour. The
	// session id is no longer shown — the app identity now lives in the rail's
	// footer — so the sidebar column of the status bar is just the band.
	bar := lipgloss.NewStyle().
		Foreground(lipgloss.Color(s.theme.BorderSubtle)).
		Background(lipgloss.Color(s.theme.BackgroundElement)).
		Render("┃")
	chatZone := conn + strings.Repeat(" ", gapA) + model
	content := chatZone + strings.Repeat(" ", bodySidebarGap) + bar

	return lipgloss.NewStyle().
		Background(lipgloss.Color(s.theme.BackgroundElement)).
		Foreground(lipgloss.Color(s.theme.Text)).
		Width(s.width).
		MaxWidth(s.width).
		Render(content)
}

// plainWidth is the display width of "prefix+value", or 0 when value is
// empty (the segment isn't rendered at all).
func plainWidth(value, prefix string) int {
	if value == "" {
		return 0
	}
	return lipgloss.Width(prefix + value)
}

func (s *StatusBar) modelSegment() string {
	if s.Model == "" {
		return ""
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(s.theme.TextMuted)).Render("model ") +
		lipgloss.NewStyle().Foreground(lipgloss.Color(s.theme.Secondary)).Render(s.Model)
}

func (s *StatusBar) renderConnDot() string {
	// Connection state lives HERE (not as top toasts) : a coloured dot, plus a
	// short label whenever the link isn't healthy so the user always knows
	// without a notification. Connected = just the dot (the green says it all).
	var dot, col, label string
	switch s.Conn {
	case ConnConnected:
		dot, col = "●", s.theme.Success
	case ConnConnecting:
		dot, col, label = "◌", s.theme.Warning, " connecting…"
	case ConnReconnecting:
		dot, col, label = "◌", s.theme.Warning, " reconnecting…"
	case ConnDisconnected:
		dot, col, label = "✗", s.theme.Error, " offline"
	default:
		dot, col = "?", s.theme.TextMuted
	}
	st := lipgloss.NewStyle().Foreground(lipgloss.Color(col))
	return st.Render(dot + label)
}

// shortID returns the last 8 chars of a session_id (or the whole
// thing if shorter) for status-bar display. UUIDs are too long to
// show in full ; the last segment is unique enough at human scale.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[len(id)-8:]
}
