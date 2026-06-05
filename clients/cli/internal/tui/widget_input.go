package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/mbathepaul/digitorn-cli/internal/theme"
)

// defaultCtxWindow is the context window shown in the footer gauge before the
// daemon's first recount lands, so the slot always reads as the context
// occupancy ("ctx 0/8.2k") instead of an unrelated char counter.
const defaultCtxWindow = 8192

// Input is the chat composer : a themed textarea with Enter-to-submit /
// Shift-Enter-for-newline. Defaults out of bubbles — we just paint the
// foregrounds with our theme.
type Input struct {
	theme *theme.Theme
	ta    textarea.Model

	Submitted bool

	// Sent-message history, shell-style. ↑/↓ walk it when the cursor is
	// on the first/last textarea line (so multi-line editing still works).
	// histIdx == len(history) means "live draft" ; draft stashes the
	// in-progress text while browsing so ↓ back past the newest restores it.
	history []string
	histIdx int
	draft   string

	// mode is the active composer mode's label (with icon), shown in the footer.
	// Empty hides the chip (app declares no modes).
	mode string

	// ctxUsed / ctxWindow drive the footer's context-occupancy gauge
	// ("ctx used/window", CTX-7) : the EXACT tokens in the model's context vs
	// the window. ctxWindow 0 (no recount yet) → the gauge shows defaultCtxWindow.
	ctxUsed   int
	ctxWindow int

	// queued is the number of messages waiting behind the in-flight turn. Shown
	// as a persistent "⋯ N queued" chip in the footer (replaces the old toast),
	// 0 hides it.
	queued int
}

// SetMode sets the active composer-mode label shown in the footer ("" hides it).
func (i *Input) SetMode(label string) { i.mode = label }

// SetQueued sets the count of messages waiting behind the current turn, shown
// in the footer. 0 hides the indicator.
func (i *Input) SetQueued(n int) { i.queued = n }

// SetContext updates the live context-occupancy gauge shown in the footer
// (used / window tokens, from the daemon's context_tokens event). window 0
// leaves the gauge on defaultCtxWindow until the first recount lands.
func (i *Input) SetContext(used, window int) {
	i.ctxUsed = used
	i.ctxWindow = window
}

func NewInput(t *theme.Theme) *Input {
	ta := textarea.New()
	ta.Placeholder = "Type a message..."
	ta.SetHeight(2)
	ta.ShowLineNumbers = false
	ta.CharLimit = 8192
	// The textarea itself stays TRANSPARENT (no background on any sub-style) so
	// the ONE fill is the panel behind it (set in View). Giving the textarea its
	// own background too produced a subtly different shade on the typing/cursor
	// line vs the panel — one surface, one fill, no two-tone.
	ta.Prompt = ""
	style := func(s textarea.StyleState) textarea.StyleState {
		s.Base = lipgloss.NewStyle()
		s.Text = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Text))
		s.CursorLine = lipgloss.NewStyle()
		s.EndOfBuffer = lipgloss.NewStyle()
		s.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color(t.TextMuted))
		return s
	}
	st := ta.Styles()
	st.Focused = style(st.Focused)
	st.Blurred = style(st.Blurred)
	ta.SetStyles(st)
	ta.Focus()
	return &Input{theme: t, ta: ta}
}

func (i *Input) SetWidth(w int) {
	if w < 20 {
		w = 20
	}
	// Reserve 2 cols for the border + 2 for the horizontal padding drawn in View().
	inner := w - 4
	if inner < 10 {
		inner = 10
	}
	i.ta.SetWidth(inner)
}

func (i *Input) Update(msg tea.Msg) tea.Cmd {
	if km, ok := msg.(tea.KeyMsg); ok {
		switch km.String() {
		case "enter":
			content := strings.TrimSpace(i.ta.Value())
			if content != "" {
				i.Submitted = true
			}
			return nil
		case "up":
			if i.historyPrev() {
				return nil
			}
		case "down":
			if i.historyNext() {
				return nil
			}
		case "ctrl+u":
			// Clear the whole composer (opencode semantics), not just to
			// the line start ; also exit history browsing.
			i.ta.Reset()
			i.histIdx = len(i.history)
			return nil
		}
	}
	var cmd tea.Cmd
	i.ta, cmd = i.ta.Update(msg)
	// Editing the buffer (any key that isn't pure cursor movement) drops
	// out of history browsing : the current text becomes a fresh draft.
	if km, ok := msg.(tea.KeyMsg); ok {
		switch km.String() {
		case "up", "down", "left", "right", "home", "end", "pgup", "pgdown":
		default:
			i.histIdx = len(i.history)
		}
	}
	return cmd
}

// Remember appends a submitted message to the history (deduping a repeat
// of the most recent) and rewinds the browse cursor to the live draft.
func (i *Input) Remember(s string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	if n := len(i.history); n == 0 || i.history[n-1] != s {
		i.history = append(i.history, s)
	}
	i.histIdx = len(i.history)
	i.draft = ""
}

// historyPrev recalls an older message. Only fires on the first textarea
// line so ↑ still moves the cursor within a multi-line draft.
func (i *Input) historyPrev() bool {
	if len(i.history) == 0 || i.ta.Line() != 0 {
		return false
	}
	if i.histIdx == len(i.history) {
		i.draft = i.ta.Value()
	}
	if i.histIdx == 0 {
		return true // already at the oldest — swallow, don't move
	}
	i.histIdx--
	i.ta.SetValue(i.history[i.histIdx])
	i.ta.CursorEnd()
	return true
}

// historyNext walks back toward the live draft. Only fires on the last
// textarea line, and only while actually browsing history.
func (i *Input) historyNext() bool {
	if i.histIdx >= len(i.history) || i.ta.Line() != i.ta.LineCount()-1 {
		return false
	}
	i.histIdx++
	if i.histIdx == len(i.history) {
		i.ta.SetValue(i.draft)
	} else {
		i.ta.SetValue(i.history[i.histIdx])
	}
	i.ta.CursorEnd()
	return true
}

// Insert drops text at the cursor. The chat routes tea.PasteMsg here
// so bracketed-paste content lands in the composer (multi-line and
// all) instead of being swallowed — paste arrives as its own message
// type, not a KeyMsg, so the textarea never sees it on its own.
func (i *Input) Insert(s string) {
	i.ta.InsertString(s)
}

// Blur / Focus toggle the composer's cursor — used while a modal (approval
// card, ask_user form) owns the keyboard so the inert composer doesn't blink.
func (i *Input) Blur()  { i.ta.Blur() }
func (i *Input) Focus() { i.ta.Focus() }

func (i *Input) Submit() (string, bool) {
	if !i.Submitted {
		return "", false
	}
	i.Submitted = false
	content := strings.TrimSpace(i.ta.Value())
	i.ta.Reset()
	return content, true
}

func (i *Input) View() string {
	// The textarea rows stay transparent (no per-row fill) so the typed text has
	// NO background of its own — the single panel fill shows behind it. The footer
	// fills its OWN line per-segment (resets between styled pieces stop a parent
	// bg from spanning it). The box provides the one panel background + border ;
	// the explicit Width lets it pad the transparent textarea rows to full width.
	inner := i.ta.View() + "\n" + i.footer()
	return lipgloss.NewStyle().
		Background(lipgloss.Color(i.theme.BackgroundElement)).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(i.theme.BorderActive)).
		Padding(0, 1).
		Width(i.ta.Width() + 4).
		Render(inner)
}

// footer is the thin line under the composer : key hints on the left, the live
// char counter on the right. Lives here (not in the sidebar) so it sits with
// the input it describes and frees the right rail for activity.
func (i *Input) footer() string {
	w := i.ta.Width() // footer lives INSIDE the padded box : match the inner width
	// EVERY segment (and the gap) carries the element background. Concatenating
	// pre-styled segments inserts ANSI resets between them, so a background set on
	// a wrapping parent doesn't survive across the resets — baking it into each
	// piece is what actually fills the whole line.
	bg := lipgloss.NewStyle().Background(lipgloss.Color(i.theme.BackgroundElement))
	muted := bg.Foreground(lipgloss.Color(i.theme.TextMuted)).Faint(true)
	hints := muted.Render("↵ send · ⇧↵ newline · ^u clear")
	// Active composer mode (when the app declares any) sits first, with a "⇧⇥"
	// hint that it cycles — always visible so the current mode is unambiguous.
	if i.mode != "" {
		chip := bg.Foreground(lipgloss.Color(i.theme.Secondary)).Bold(true).
			Render("[" + i.mode + "]")
		hints = chip + muted.Render(" ⇧⇥  ") + hints
	}
	// Pending-queue indicator : persistent while messages wait behind the turn.
	if i.queued > 0 {
		q := bg.Foreground(lipgloss.Color(i.theme.Accent)).Bold(true).
			Render(fmt.Sprintf("⋯ %d queued", i.queued))
		hints = q + muted.Render("   ") + hints
	}
	// Right counter : always the context-occupancy gauge "ctx used/window". It
	// climbs as the conversation grows and drops when the daemon compacts. Until
	// the first recount lands (ctxWindow==0) it shows the default window, so the
	// slot is the context from the first frame rather than an unrelated char
	// counter that merely looked like one.
	window := i.ctxWindow
	if window <= 0 {
		window = defaultCtxWindow
	}
	cc := i.theme.TextMuted
	switch {
	case i.ctxUsed*100 >= window*90:
		cc = i.theme.Error
	case i.ctxUsed*100 >= window*75:
		cc = i.theme.Warning
	}
	counter := bg.Foreground(lipgloss.Color(cc)).Faint(true).
		Render(fmt.Sprintf("ctx %s/%s", humanizeTokens(i.ctxUsed), humanizeTokens(window)))

	gap := w - lipgloss.Width(hints) - lipgloss.Width(counter)
	if gap < 1 {
		gap = 1
	}
	return hints + bg.Render(strings.Repeat(" ", gap)) + counter
}

func (i *Input) Height() int {
	return i.ta.Height() + 3 // textarea + top/bottom border + footer line
}

func (i *Input) Value() string {
	return i.ta.Value()
}

func (i *Input) CharLimit() int {
	return i.ta.CharLimit
}
