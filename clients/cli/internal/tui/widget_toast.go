package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Toasts are transient, self-dismissing notifications stacked at the
// top-right of the chat — opencode style. They never steal focus ; an
// animation tick prunes them once expired. Used for ephemeral feedback
// (theme switched, command result) that doesn't belong in the
// persistent transcript.

type toastLevel int

const (
	toastInfo toastLevel = iota
	toastSuccess
	toastWarning
	toastError
)

type toastItem struct {
	text   string
	level  toastLevel
	expiry time.Time
}

const (
	toastTTL = 4 * time.Second
	toastMax = 4
)

// addToast pushes a notification. The caller's Update branch must return
// s.ensureTick() so the prune loop is running.
func (s *ChatScreen) addToast(level toastLevel, text string) {
	s.toasts = append(s.toasts, toastItem{
		text:   text,
		level:  level,
		expiry: time.Now().Add(toastTTL),
	})
	if len(s.toasts) > toastMax {
		s.toasts = s.toasts[len(s.toasts)-toastMax:]
	}
}

// pruneToasts drops expired notifications. Returns true if any remain.
func (s *ChatScreen) pruneToasts() bool {
	now := time.Now()
	kept := s.toasts[:0]
	for _, t := range s.toasts {
		if t.expiry.After(now) {
			kept = append(kept, t)
		}
	}
	s.toasts = kept
	return len(s.toasts) > 0
}

func (s *ChatScreen) toastColor(l toastLevel) (color, icon string) {
	switch l {
	case toastSuccess:
		return s.theme.Success, "✓"
	case toastWarning:
		return s.theme.Warning, "!"
	case toastError:
		return s.theme.Error, "✗"
	default:
		return s.theme.Info, "i"
	}
}

func (s *ChatScreen) renderToast(t toastItem, maxW int) string {
	col, icon := s.toastColor(t.level)
	body := oneLine(t.text, maxW-4)
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(col)).
		Background(lipgloss.Color(s.theme.BackgroundPanel)).
		Foreground(lipgloss.Color(s.theme.Text)).
		Padding(0, 1).
		MaxWidth(maxW).
		Render(lipgloss.NewStyle().Foreground(lipgloss.Color(col)).Render(icon) + " " + body)
}

// overlayToasts paints the toast stack over the top-right of an already-
// rendered base frame, compositing line-by-line with ANSI-aware slicing
// so the chat underneath (and to the left of) each toast is preserved.
func (s *ChatScreen) overlayToasts(base string) string {
	if len(s.toasts) == 0 || s.width == 0 {
		return base
	}
	maxW := s.width / 3
	if maxW < 26 {
		maxW = 26
	}
	cards := make([]string, 0, len(s.toasts))
	for _, t := range s.toasts {
		cards = append(cards, s.renderToast(t, maxW))
	}
	stack := lipgloss.JoinVertical(lipgloss.Right, cards...)
	stackLines := strings.Split(stack, "\n")
	stackW := lipgloss.Width(stack)
	// Anchor to the bottom-right of the CHAT BODY (not the whole width — that
	// would sit over the sidebar), just above the composer : notices appear where
	// the eye already is, instead of being dumped at the top.
	bodyW := s.width - computeSidebarWidth(s.width) - bodySidebarGap
	if bodyW < 30 {
		bodyW = 30
	}
	x := bodyW - stackW - 1
	if x < 0 {
		x = 0
	}
	lines := strings.Split(base, "\n")
	startY := len(lines) - len(stackLines) - (s.input.Height() + 1) // clear the input
	if startY < 0 {
		startY = 0
	}
	for i, tline := range stackLines {
		y := startY + i
		if y < 0 || y >= len(lines) {
			continue
		}
		bl := lines[y]
		blW := lipgloss.Width(bl)
		left := ansi.Truncate(bl, x, "")
		if w := lipgloss.Width(left); w < x {
			left += strings.Repeat(" ", x-w)
		}
		rest := ""
		if end := x + lipgloss.Width(tline); end < blW {
			rest = ansi.Cut(bl, end, blW)
		}
		lines[y] = left + tline + rest
	}
	return strings.Join(lines, "\n")
}

// ---- shimmer (waiting indicator) ----------------------------------

// shimmering reports whether the "working…" waiting indicator should show :
// any time a turn is in flight. It stays on for the WHOLE turn — including
// while a tool chip executes. The chip's own "running…" suffix is static text
// (it only ticks the duration), so without this the entire tool phase looked
// frozen ; the sweeping indicator is the single source of "still working"
// feedback from send to turn_ended.
func (s *ChatScreen) shimmering() bool {
	return s.pendingTurn
}

// shimmerGlyphs is the in-place "pulsing diamond" the working indicator cycles
// through : a point swells into a filled diamond, tumbles 45° to a square at the
// peak, then eases back down — dot, ring, outline, fish-eye, diamond, square,
// diamond, fish-eye, outline, ring. A breathing pulse that also turns, distinct
// from Claude's sparkle.
var shimmerGlyphs = []string{"·", "◦", "◇", "◈", "◆", "◼", "◆", "◈", "◇", "◦"}

// shimmerSlow holds each frame for this many animation ticks, so the diamond
// breathes calmly instead of racing (tick is ~120ms, so 2 ≈ 240ms per frame).
const shimmerSlow = 2

// renderShimmer paints just the in-place orbiting glyph, animated by
// shimmerFrame — no label, no dots, the motion alone signals "still working".
func (s *ChatScreen) renderShimmer() string {
	hot := lipgloss.NewStyle().Foreground(lipgloss.Color(s.theme.Primary)).Bold(true)
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color(s.theme.TextMuted))
	// The trailing text (compaction note, token counter) is rendered faint so it
	// recedes and the animated figure stays the focal point of the line.
	faint := muted.Faint(true)
	glyph := shimmerGlyphs[(s.shimmerFrame/shimmerSlow)%len(shimmerGlyphs)]
	line := muted.Render("  ") + hot.Render(glyph)
	// Live compaction indicator : while the daemon is condensing history to hold
	// the window, say so explicitly so the user knows what the pause is.
	if s.compacting {
		line += faint.Render("  ⟢ compacting context…")
		return line
	}
	// Live token counter (CTX-7.5) : prefer the daemon's server-authoritative
	// running count ; fall back to the local chars/4 estimate before the first
	// count lands. It climbs at the rhythm tokens arrive during generation.
	// One authoritative live counter : turnTokens is fed by LiveOutputTokens
	// from BOTH assistant_delta (text) and streaming tool_call events (text +
	// tool-call arguments), so tool tokens are always included and never drop
	// out when a tool finishes.
	tok := s.turnTokens
	if tok == 0 && s.turnChars > 0 {
		tok = s.turnChars / 4
	}
	if tok > 0 {
		// "~" marks the live estimate ; it drops once the exact provider
		// usage lands (turnTokensExact) so the user always knows which is which.
		prefix := "~"
		if s.turnTokensExact {
			prefix = ""
		}
		line += faint.Render(fmt.Sprintf("  %s%s tokens", prefix, humanizeTokens(tok)))
	}
	return line
}
