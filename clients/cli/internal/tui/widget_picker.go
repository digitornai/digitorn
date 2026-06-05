package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/mbathepaul/digitorn-cli/internal/theme"
)

// PickerItem is one row in the picker. ID is returned on selection ;
// Label is the prominent text shown ; Hint is small secondary text
// (timestamps, counts, etc.) shown to the right of Label.
type PickerItem struct {
	ID    string
	Label string
	Hint  string
}

// Picker is a fullscreen-ish overlay list. Arrow keys / j-k move,
// Enter selects, Esc cancels. Used for /sessions, /apps, etc.
// Generic over what the items mean — the chat screen knows what to
// do with the selected ID.
type Picker struct {
	theme  *theme.Theme
	title  string
	items  []PickerItem
	cursor int // index into matches, not items
	width  int
	height int

	// query filters the list (fuzzy) ; matches holds the item indices that
	// survive, best-ranked first. Typing edits the query, arrows navigate the
	// filtered view — the opencode dialog model.
	query   string
	matches []int

	// deletable enables the two-press delete affordance (sessions only).
	// pendingDelete is the index (into items) armed for confirmation, or -1.
	deletable     bool
	pendingDelete int

	// loading shows a spinner instead of the list while the items are being
	// fetched (a session/app list can be slow to load). spin is the animation
	// frame, set by the screen each tick. SetItems clears loading.
	loading bool
	spin    int
}

// NewLoadingPicker returns a picker shown immediately in a spinner state, before
// its items are fetched. Call SetItems when they arrive.
func NewLoadingPicker(t *theme.Theme, title string) *Picker {
	return &Picker{theme: t, title: title, pendingDelete: -1, loading: true}
}

// SetItems fills a (typically loading) picker with its rows and clears the
// loading state.
func (p *Picker) SetItems(items []PickerItem, deletable bool) {
	p.items = items
	p.deletable = deletable
	p.loading = false
	p.cursor = 0
	p.refilter()
}

// SetSpin updates the loading spinner's animation frame.
func (p *Picker) SetSpin(frame int) { p.spin = frame }

// Loading reports whether the picker is still fetching its items.
func (p *Picker) Loading() bool { return p.loading }

// PickerAction is what Update returns. Set Selected when the user pressed
// Enter on a valid item ; Deleted when they confirmed a delete ; Cancelled
// on Esc. All zero = the key was consumed but no terminal action yet.
type PickerAction struct {
	Selected  string
	Deleted   string
	Cancelled bool
}

func NewPicker(t *theme.Theme, title string, items []PickerItem) *Picker {
	p := &Picker{theme: t, title: title, items: items, pendingDelete: -1}
	p.refilter()
	return p
}

// refilter recomputes matches from the current query and clamps the cursor.
func (p *Picker) refilter() {
	if p.query == "" {
		p.matches = p.matches[:0]
		for i := range p.items {
			p.matches = append(p.matches, i)
		}
	} else {
		labels := make([]string, len(p.items))
		for i, it := range p.items {
			labels[i] = it.Label + " " + it.Hint
		}
		p.matches = fuzzyFilter(p.query, labels)
	}
	if p.cursor >= len(p.matches) {
		p.cursor = len(p.matches) - 1
	}
	if p.cursor < 0 {
		p.cursor = 0
	}
}

// selectedItem returns the item under the cursor (post-filter), ok=false when
// the filtered list is empty.
func (p *Picker) selectedItem() (PickerItem, bool) {
	if p.cursor < 0 || p.cursor >= len(p.matches) {
		return PickerItem{}, false
	}
	return p.items[p.matches[p.cursor]], true
}

func (p *Picker) SetSize(w, h int) {
	p.width = w
	p.height = h
}

// Update handles keystrokes. Returns the action OR a zero-value if the
// key isn't terminal (still navigating).
func (p *Picker) Update(msg tea.Msg) PickerAction {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return PickerAction{}
	}
	key := km.String()
	if p.loading {
		if key == "esc" || key == "ctrl+c" {
			return PickerAction{Cancelled: true}
		}
		return PickerAction{}
	}
	// Any key other than a repeated delete cancels a pending confirmation.
	if key != "ctrl+x" && p.pendingDelete >= 0 {
		p.pendingDelete = -1
	}
	switch key {
	case "esc", "ctrl+c":
		return PickerAction{Cancelled: true}
	case "enter":
		if it, ok := p.selectedItem(); ok {
			return PickerAction{Selected: it.ID}
		}
		return PickerAction{Cancelled: true}
	case "ctrl+x":
		// Two-press delete (ctrl+x, not "x", so letters stay free for the
		// search query). First press arms the highlighted row, second confirms.
		if !p.deletable {
			return PickerAction{}
		}
		it, ok := p.selectedItem()
		if !ok {
			return PickerAction{}
		}
		idx := p.matches[p.cursor]
		if p.pendingDelete == idx {
			p.pendingDelete = -1
			return PickerAction{Deleted: it.ID}
		}
		p.pendingDelete = idx
	case "up", "ctrl+p":
		if p.cursor > 0 {
			p.cursor--
		}
	case "down", "ctrl+n":
		if p.cursor < len(p.matches)-1 {
			p.cursor++
		}
	case "home":
		p.cursor = 0
	case "end":
		p.cursor = len(p.matches) - 1
	case "pgup":
		if p.cursor -= 10; p.cursor < 0 {
			p.cursor = 0
		}
	case "pgdown":
		if p.cursor += 10; p.cursor > len(p.matches)-1 {
			p.cursor = len(p.matches) - 1
		}
	case "backspace":
		if q := []rune(p.query); len(q) > 0 {
			p.query = string(q[:len(q)-1])
			p.refilter()
		}
	case "ctrl+u":
		if p.query != "" {
			p.query = ""
			p.refilter()
		}
	case "space":
		p.query += " "
		p.refilter()
	default:
		// A printable single rune extends the search query (fuzzy filter). A
		// modified key stringifies as e.g. "ctrl+a" (len>1), so a lone rune
		// already implies no modifier.
		if r := []rune(key); len(r) == 1 {
			p.query += key
			p.refilter()
		}
	}
	return PickerAction{}
}

func (p *Picker) View() string {
	if p.width == 0 || p.height == 0 {
		return ""
	}
	th := p.theme
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color(th.TextMuted))
	faint := muted.Faint(true)

	// listW is the row content width ; the box auto-sizes to it + chrome (border 2
	// + padding 4). Capped so the overlay stays readable on a wide terminal, and
	// computed directly (not from the box Width, whose total-vs-content semantics
	// are ambiguous and made rows overflow → the version hint wrapped).
	listW := p.width - 8
	if listW > 96 {
		listW = 96
	}
	if listW < 24 {
		listW = 24
	}

	title := lipgloss.NewStyle().Foreground(lipgloss.Color(th.Primary)).Bold(true).Render(p.title)

	if p.loading {
		glyph := shimmerGlyphs[(p.spin/shimmerSlow)%len(shimmerGlyphs)]
		spinner := lipgloss.NewStyle().Foreground(lipgloss.Color(th.Primary)).Render(glyph) +
			muted.Render("  loading…")
		body := make([]string, 0, 13)
		for i := 0; i < 5; i++ {
			body = append(body, "")
		}
		body = append(body, lipgloss.PlaceHorizontal(listW, lipgloss.Center, spinner))
		for i := 0; i < 5; i++ {
			body = append(body, "")
		}
		body = append(body, faint.Render("esc cancel"))
		rows := append([]string{title, ""}, body...)
		box := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(th.BorderActive)).
			Padding(1, 2).
			Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
		return lipgloss.Place(p.width, p.height, lipgloss.Center, lipgloss.Center, box,
			lipgloss.WithWhitespaceChars(" "))
	}

	hint := "type to filter · ↑↓ navigate · ⏎ select · esc cancel"
	if p.deletable {
		hint += " · ^x delete"
	}
	hintBar := faint.Render(hint)

	// Search line : prompt + live query + cursor, with a count (matches/total)
	// right-aligned so it's visible even with thousands of items.
	queryText := p.query
	if queryText == "" {
		queryText = muted.Italic(true).Render("filter…")
	} else {
		queryText = lipgloss.NewStyle().Foreground(lipgloss.Color(th.Text)).Render(queryText)
	}
	left := lipgloss.NewStyle().Foreground(lipgloss.Color(th.Accent)).Render("› ") +
		queryText + lipgloss.NewStyle().Foreground(lipgloss.Color(th.Primary)).Render("▌")
	countStr := strconv.Itoa(len(p.items))
	if p.query != "" {
		countStr = fmt.Sprintf("%d/%d", len(p.matches), len(p.items))
	}
	count := muted.Render(countStr)
	if g := listW - lipgloss.Width(left) - lipgloss.Width(count); g > 0 {
		left += strings.Repeat(" ", g)
	}
	searchLine := left + count

	// List area : only the rows that FIT are rendered, scrolled to keep the
	// cursor visible. Reserve 2 rows for the ↑/↓ overflow markers so the box
	// height stays stable as you scroll. fixed chrome = border(2) + pad(2) +
	// title(1) + search(1) + blank(1) + blank(1) + hint(1) = 9.
	listH := p.height - 9
	if listH < 3 {
		listH = 3
	}
	// Each app row is followed by a blank spacer line (vertical padding), so an
	// item occupies 2 lines. Halve the capacity (minus the 2 marker lines) so the
	// list still fits the available height.
	itemCap := (listH - 2) / 2
	if itemCap < 1 {
		itemCap = 1
	}

	var listRows []string
	switch {
	case len(p.items) == 0:
		listRows = append(listRows, muted.Italic(true).Render("(empty)"))
	case len(p.matches) == 0:
		listRows = append(listRows, muted.Italic(true).Render("(no matches)"))
	default:
		total := len(p.matches)
		start := 0
		if total > itemCap {
			start = p.cursor - itemCap/2 // keep the cursor roughly centred
			if start < 0 {
				start = 0
			}
			if start > total-itemCap {
				start = total - itemCap
			}
		}
		end := start + itemCap
		if end > total {
			end = total
		}
		// Top overflow marker (always a line, blank when none → stable height).
		if start > 0 {
			listRows = append(listRows, faint.Render(fmt.Sprintf("  ↑ %d more", start)))
		} else {
			listRows = append(listRows, "")
		}
		for vi := start; vi < end; vi++ {
			idx := p.matches[vi]
			listRows = append(listRows, p.renderRow(p.items[idx], vi == p.cursor, idx == p.pendingDelete, listW))
			listRows = append(listRows, "") // vertical padding between app rows
		}
		if end < total {
			listRows = append(listRows, faint.Render(fmt.Sprintf("  ↓ %d more", total-end)))
		} else {
			listRows = append(listRows, "")
		}
	}

	// Give the body a minimum height (blank-padded) so the dialog keeps a
	// comfortable size even with just a few items — capped by what the screen
	// allows so it never overflows.
	minBody := 12
	if minBody > listH {
		minBody = listH
	}
	for len(listRows) < minBody {
		listRows = append(listRows, "")
	}

	rows := append([]string{title, searchLine, ""}, listRows...)
	rows = append(rows, "", hintBar)

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(th.BorderActive)).
		Padding(1, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left, rows...))

	return lipgloss.Place(p.width, p.height, lipgloss.Center, lipgloss.Center, box,
		lipgloss.WithWhitespaceChars(" "))
}

func (p *Picker) renderRow(it PickerItem, selected, pending bool, width int) string {
	label := it.Label
	hint := it.Hint
	// While armed for deletion, the row swaps its hint for a confirm prompt.
	if pending {
		hint = "delete? ^x to confirm · esc cancel"
	}
	// Truncate label if too long ; hint is right-aligned beside it. Rune/display
	// aware (lipgloss.Width + truncate) so multibyte names aren't cut mid-rune.
	// 2-space padding each side so the label/hint aren't glued to the row edge
	// (and the selection highlight has internal breathing room).
	const pad = 2
	hintW := lipgloss.Width(hint)
	maxLabel := width - hintW - 2*pad - 2
	if maxLabel < 8 {
		maxLabel = 8
	}
	if lipgloss.Width(label) > maxLabel {
		label = truncate(label, maxLabel)
	}
	gap := width - lipgloss.Width(label) - hintW - 2*pad
	if gap < 1 {
		gap = 1
	}
	p1 := strings.Repeat(" ", pad)
	rowText := p1 + label + strings.Repeat(" ", gap) + hint + p1

	switch {
	case pending:
		return lipgloss.NewStyle().
			Foreground(lipgloss.Color(p.theme.Background)).
			Background(lipgloss.Color(p.theme.Error)).
			Bold(true).
			Render(rowText)
	case selected:
		return lipgloss.NewStyle().
			Foreground(lipgloss.Color(p.theme.Background)).
			Background(lipgloss.Color(p.theme.Primary)).
			Bold(true).
			Render(rowText)
	}
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color(p.theme.Text)).
		Render(rowText)
}
