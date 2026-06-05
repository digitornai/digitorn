package tui

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/mbathepaul/digitorn-cli/internal/theme"
)

// AskForm renders an ask_user question and collects the answer. Unlike the
// SG-5 tool ApprovalPrompt (a fixed y/a/n decision), a question carries one of
// five interaction shapes that the daemon already emits :
//
//   - free text          → question only
//   - content review     → an editable markdown blob to revise
//   - single choice       → pick one of choices
//   - multiple choice     → pick several of choices (allow_multiple)
//   - structured form     → a list of typed fields
//
// Every shape is modelled as a list of fields so the rendering and key
// handling stay uniform ; only the answer assembly differs (a lone field
// returns its raw value, a form returns a JSON object of name→value — the
// shape formatAskUserResponse expects on the daemon).
type AskForm struct {
	theme    *theme.Theme
	id       string
	question string
	isForm   bool // assemble the answer as a JSON object
	fields   []*askField
	active   int

	ta textarea.Model // shared editor for the focused text-like field

	// bg is the modal's base style carrying the panel background ; every inner
	// span derives from it so the fill is solid (ANSI resets between concatenated
	// styled spans would otherwise punch holes in a background set only on the
	// wrapping box). Set at the top of Card.
	bg lipgloss.Style

	// qScroll is the top line of the visible window into a long (wrapped)
	// question ; qScrollable is set during render when the question overflows
	// so the footer can advertise PgUp/PgDn.
	qScroll     int
	qScrollable bool

	submitting bool
}

// fieldKind is the interaction of a single field.
type fieldKind int

const (
	fieldText        fieldKind = iota // single-line text
	fieldTextarea                     // multi-line text (content / form textarea)
	fieldSelect                       // pick one of options
	fieldMultiSelect                  // pick several of options
	fieldNumber                       // numeric text
	fieldBoolean                      // yes / no toggle
)

type askField struct {
	name    string
	label   string
	kind    fieldKind
	options []string

	// state
	text    string       // text / textarea / number
	selIdx  int          // cursor for select ; 0/1 for boolean
	checked map[int]bool // multiselect picks
}

// NewAskForm builds the form from an approval_request payload whose
// kind=="question". The payload mirrors sessionstore.ApprovalPayload : id, the
// question in "reason", and a nested "payload" with content / choices /
// allow_multiple / form. Returns nil when there is no id to answer.
func NewAskForm(t *theme.Theme, payload map[string]any) *AskForm {
	id := mapStr(payload, "id")
	if id == "" {
		return nil
	}
	f := &AskForm{theme: t, id: id, question: mapStr(payload, "reason")}

	inner, _ := payload["payload"].(map[string]any)
	content := mapStr(inner, "content")
	choices := stringSlice(inner["choices"])
	allowMultiple, _ := inner["allow_multiple"].(bool)
	form := mapSlice(inner["form"])

	switch {
	case len(form) > 0:
		f.isForm = true
		for _, raw := range form {
			f.fields = append(f.fields, fieldFromSchema(raw))
		}
	case len(choices) > 0 && allowMultiple:
		f.fields = []*askField{{kind: fieldMultiSelect, options: choices, checked: map[int]bool{}}}
	case len(choices) > 0:
		f.fields = []*askField{{kind: fieldSelect, options: choices}}
	case content != "":
		f.fields = []*askField{{kind: fieldTextarea, text: content}}
	default:
		f.fields = []*askField{{kind: fieldText}}
	}
	if len(f.fields) == 0 { // a form schema that parsed to nothing
		f.fields = []*askField{{kind: fieldText}}
	}

	f.ta = newFieldEditor(t)
	f.focus(0)
	return f
}

// fieldFromSchema decodes one form field. Unknown types fall back to text ; an
// options list promotes a bare field to a select.
func fieldFromSchema(raw map[string]any) *askField {
	fld := &askField{
		name:    mapStr(raw, "name"),
		label:   mapStr(raw, "label"),
		options: stringSlice(raw["options"]),
		checked: map[int]bool{},
	}
	if fld.label == "" {
		fld.label = fld.name
	}
	switch strings.ToLower(mapStr(raw, "type")) {
	case "textarea", "multiline", "text_area":
		fld.kind = fieldTextarea
	case "select", "choice", "dropdown", "enum":
		fld.kind = fieldSelect
	case "multiselect", "multi_select", "checkboxes", "multichoice":
		fld.kind = fieldMultiSelect
	case "number", "int", "integer", "float":
		fld.kind = fieldNumber
	case "boolean", "bool", "confirm", "toggle":
		fld.kind = fieldBoolean
	default:
		if len(fld.options) > 0 {
			fld.kind = fieldSelect
		} else {
			fld.kind = fieldText
		}
	}
	// Seed a default value.
	switch v := raw["default"].(type) {
	case string:
		fld.text = v
		fld.selIdx = indexOf(fld.options, v)
	case bool:
		if v {
			fld.selIdx = 1
		}
	case float64:
		fld.text = strconv.FormatFloat(v, 'f', -1, 64)
	}
	return fld
}

func newFieldEditor(t *theme.Theme) textarea.Model {
	ta := textarea.New()
	ta.ShowLineNumbers = false
	ta.CharLimit = 8192
	ta.SetHeight(1)
	return ta
}

func (f *AskForm) ID() string { return f.id }

// focus moves the active field, loading its text into the shared editor and
// sizing the editor for single- vs multi-line fields.
func (f *AskForm) focus(i int) {
	if i < 0 {
		i = 0
	}
	if i >= len(f.fields) {
		i = len(f.fields) - 1
	}
	f.active = i
	fld := f.fields[i]
	if fld.kind == fieldText || fld.kind == fieldTextarea || fld.kind == fieldNumber {
		h := 1
		if fld.kind == fieldTextarea {
			h = 4
		}
		f.ta.SetHeight(h)
		f.ta.SetValue(fld.text)
		f.ta.CursorEnd()
		f.ta.Focus()
	} else {
		f.ta.Blur()
	}
}

// saveActive copies the editor buffer back into the active field (text kinds).
func (f *AskForm) saveActive() {
	fld := f.fields[f.active]
	if fld.kind == fieldText || fld.kind == fieldTextarea || fld.kind == fieldNumber {
		fld.text = f.ta.Value()
	}
}

// AskFormAction is the outcome of one Update : at most one of Cancelled /
// Submit is set. Answer carries the assembled reply when Submit is true.
type AskFormAction struct {
	Cancelled bool
	Submit    bool
	Answer    string
}

func (f *AskForm) SetWidth(w int) {
	inner := w - 8
	if inner < 16 {
		inner = 16
	}
	if inner > 96 {
		inner = 96
	}
	f.ta.SetWidth(inner)
}

// Update consumes one key. Navigation : tab/shift+tab move between form fields.
// Submit : ctrl+d always ; enter when the active field is not a multi-line
// editor (there enter inserts a newline). Esc cancels.
func (f *AskForm) Update(msg tea.Msg) AskFormAction {
	if f.submitting {
		return AskFormAction{}
	}
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		// Let the editor consume non-key messages (paste, etc.).
		f.ta, _ = f.ta.Update(msg)
		return AskFormAction{}
	}

	fld := f.fields[f.active]
	key := km.String()

	switch key {
	case "esc":
		return AskFormAction{Cancelled: true}
	case "ctrl+d":
		f.saveActive()
		return AskFormAction{Submit: true, Answer: f.assemble()}
	case "pgup", "ctrl+up":
		f.qScroll-- // scroll the (long) question up ; clamped in renderQuestion
		return AskFormAction{}
	case "pgdown", "ctrl+down":
		f.qScroll++
		return AskFormAction{}
	case "tab", "shift+tab":
		if len(f.fields) > 1 {
			f.saveActive()
			if key == "tab" {
				f.focus((f.active + 1) % len(f.fields))
			} else {
				f.focus((f.active - 1 + len(f.fields)) % len(f.fields))
			}
			return AskFormAction{}
		}
	}

	switch fld.kind {
	case fieldSelect, fieldBoolean:
		n := len(fld.options)
		if fld.kind == fieldBoolean {
			n = 2
		}
		switch key {
		case "up", "k", "left":
			if fld.selIdx > 0 {
				fld.selIdx--
			}
		case "down", "j", "right":
			if fld.selIdx < n-1 {
				fld.selIdx++
			}
		case "enter":
			f.saveActive()
			return AskFormAction{Submit: true, Answer: f.assemble()}
		}
		return AskFormAction{}

	case fieldMultiSelect:
		switch key {
		case "up", "k":
			if fld.selIdx > 0 {
				fld.selIdx--
			}
		case "down", "j":
			if fld.selIdx < len(fld.options)-1 {
				fld.selIdx++
			}
		case "space", " ", "x":
			fld.checked[fld.selIdx] = !fld.checked[fld.selIdx]
		case "enter":
			f.saveActive()
			return AskFormAction{Submit: true, Answer: f.assemble()}
		}
		return AskFormAction{}

	default: // text / textarea / number
		// Single-line fields submit on enter ; the multi-line editor keeps it
		// as a newline (submit with ctrl+d).
		if key == "enter" && fld.kind != fieldTextarea {
			f.saveActive()
			return AskFormAction{Submit: true, Answer: f.assemble()}
		}
		if fld.kind == fieldNumber && isPrintableKey(km) && !isNumericKey(key) {
			return AskFormAction{} // reject non-numeric input
		}
		f.ta, _ = f.ta.Update(msg)
		f.saveActive()
		return AskFormAction{}
	}
}

// assemble turns the field state into the reply string the daemon expects.
func (f *AskForm) assemble() string {
	if f.isForm {
		out := map[string]any{}
		for _, fld := range f.fields {
			key := fld.name
			if key == "" {
				key = fld.label
			}
			out[key] = fld.value()
		}
		b, err := json.Marshal(out)
		if err != nil {
			return ""
		}
		return string(b)
	}
	return f.fields[0].rawValue()
}

// value returns a typed answer for a form field (string / bool / number).
func (fld *askField) value() any {
	switch fld.kind {
	case fieldBoolean:
		return fld.selIdx == 1
	case fieldNumber:
		if n, err := strconv.ParseFloat(strings.TrimSpace(fld.text), 64); err == nil {
			return n
		}
		return strings.TrimSpace(fld.text)
	case fieldSelect:
		if fld.selIdx >= 0 && fld.selIdx < len(fld.options) {
			return fld.options[fld.selIdx]
		}
		return ""
	case fieldMultiSelect:
		return fld.picked()
	default:
		return fld.text
	}
}

// rawValue is the lone-field (non-form) reply : the chosen text, the picked
// option, or comma-joined multi-select picks.
func (fld *askField) rawValue() string {
	switch fld.kind {
	case fieldSelect:
		if fld.selIdx >= 0 && fld.selIdx < len(fld.options) {
			return fld.options[fld.selIdx]
		}
		return ""
	case fieldMultiSelect:
		return strings.Join(fld.picked(), ", ")
	case fieldBoolean:
		if fld.selIdx == 1 {
			return "yes"
		}
		return "no"
	default:
		return strings.TrimSpace(fld.text)
	}
}

func (fld *askField) picked() []string {
	var out []string
	for i, opt := range fld.options {
		if fld.checked[i] {
			out = append(out, opt)
		}
	}
	return out
}

// Card renders the question + its control docked where the composer normally
// sits. opencode-clean : a subtle left accent bar (not a full box), an accent
// "?" heading, and indented fields — consistent with the chat's un-boxed look.
func (f *AskForm) Card(width int) string {
	innerW := width - 7 // -6 box chrome, -1 reserved for the drop-shadow column
	if innerW < 28 {
		innerW = 28
	}
	if innerW > 96 {
		innerW = 96
	}
	f.SetWidth(innerW)

	// Elevated panel : a filled background (lighter than the chat's bare bg) +
	// a rounded accent border read as a raised modal. Every inner span derives
	// from f.bg so the fill is solid across ANSI resets.
	panel := lipgloss.Color(f.theme.BackgroundElement)
	f.bg = lipgloss.NewStyle().Background(panel)
	muted := f.bg.Foreground(lipgloss.Color(f.theme.TextMuted))

	// Title strip : announces the modal and is the "question" label, visually
	// distinct from the answer zone below the divider.
	title := f.bg.Foreground(lipgloss.Color(f.theme.Accent)).Bold(true).Render("?") +
		muted.Bold(true).Render("  QUESTION")
	divider := f.bg.Foreground(lipgloss.Color(f.theme.BorderSubtle)).Render(strings.Repeat("─", innerW))

	rows := []string{title, "", f.renderQuestion(innerW), divider, ""}
	if f.isForm {
		for i, fld := range f.fields {
			if i > 0 {
				rows = append(rows, "")
			}
			rows = append(rows, f.renderField(fld, i == f.active, innerW))
		}
	} else {
		rows = append(rows, f.renderField(f.fields[0], true, innerW))
	}
	rows = append(rows, "", f.footer(muted))

	// Fill every line to the inner width so the background paints edge-to-edge.
	for i := range rows {
		rows[i] = padLinesBg(rows[i], innerW, f.bg)
	}

	content := lipgloss.JoinVertical(lipgloss.Left, rows...)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(f.theme.Accent)).
		BorderBackground(lipgloss.Color(f.theme.Background)).
		Background(panel).
		Padding(1, 2).
		Width(innerW).
		Render(content)
	return dropShadow(box, f.theme)
}

// dropShadow paints a soft drop shadow under a docked modal (offset one cell down
// + right) so it reads as raised off the chat. The shadow is a column of light-
// shade glyphs on the box's right edge (skipping the top row, for the offset)
// plus a row beneath it, in a dim grey — visible on any terminal background.
// Shared by the ask_user form and the approval prompt ; both reserve one column
// for it (innerW = width-7) so it never bleeds into the sidebar.
// padLinesBg right-pads EVERY line of s to w cells with the panel background, so
// a multi-line block (wrapped question, params) fills edge-to-edge — padding only
// the last line would leave bg holes on the rows above it. JoinVertical then adds
// no plain-bg padding of its own since all lines are already w wide.
func padLinesBg(s string, w int, bg lipgloss.Style) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if d := w - lipgloss.Width(ln); d > 0 {
			lines[i] = ln + bg.Render(strings.Repeat(" ", d))
		}
	}
	return strings.Join(lines, "\n")
}

func dropShadow(box string, t *theme.Theme) string {
	sh := lipgloss.NewStyle().Foreground(lipgloss.Color(t.BorderSubtle)).Faint(true)
	lines := strings.Split(box, "\n")
	if len(lines) == 0 {
		return box
	}
	w := lipgloss.Width(lines[0])
	for i := range lines {
		if i == 0 {
			continue // top row : no shadow yet (the light comes from top-left)
		}
		lines[i] += sh.Render("░")
	}
	bottom := " " + sh.Render(strings.Repeat("░", w))
	return strings.Join(lines, "\n") + "\n" + bottom
}

// maxQuestionLines bounds the question heading's HEIGHT so the answer fields
// (and the input) always stay on screen. A question longer than this isn't
// truncated — it scrolls (PgUp/PgDn), with a position indicator.
const maxQuestionLines = 4

// renderQuestion lays out the "? <question>" heading. The question wraps to the
// card width with continuation lines hang-indented ; when it's taller than
// maxQuestionLines it becomes a scrollable window (clamped to qScroll) with a
// muted "lines X–Y / N" indicator so the user can read all of it without the
// controls being pushed away.
func (f *AskForm) renderQuestion(width int) string {
	muted := f.bg.Foreground(lipgloss.Color(f.theme.TextMuted))
	wrapped := strings.Split(f.bg.
		Foreground(lipgloss.Color(f.theme.Text)).Bold(true).
		Width(width).
		Render(orDash(f.question)), "\n")

	total := len(wrapped)
	f.qScrollable = total > maxQuestionLines
	if !f.qScrollable {
		f.qScroll = 0
	} else {
		// Clamp the scroll window to [0, total-maxQuestionLines].
		if f.qScroll > total-maxQuestionLines {
			f.qScroll = total - maxQuestionLines
		}
		if f.qScroll < 0 {
			f.qScroll = 0
		}
	}

	end := total
	if f.qScrollable {
		end = f.qScroll + maxQuestionLines
	}
	var out []string
	for i := f.qScroll; i < end; i++ {
		out = append(out, wrapped[i])
	}
	if f.qScrollable {
		out = append(out, muted.Render(fmt.Sprintf("↕ lines %d–%d / %d · PgUp/PgDn",
			f.qScroll+1, end, total)))
	}
	return strings.Join(out, "\n")
}

func (f *AskForm) renderField(fld *askField, focused bool, width int) string {
	muted := f.bg.Foreground(lipgloss.Color(f.theme.TextMuted))
	label := ""
	if fld.label != "" {
		lab := muted
		if focused {
			lab = f.bg.Foreground(lipgloss.Color(f.theme.Text)).Bold(true)
		}
		label = lab.Render(fld.label) + "\n"
	}

	switch fld.kind {
	case fieldSelect:
		return label + f.renderChoices(fld, focused, false, width)
	case fieldMultiSelect:
		return label + f.renderChoices(fld, focused, true, width)
	case fieldBoolean:
		yn := &askField{kind: fieldSelect, options: []string{"No", "Yes"}, selIdx: fld.selIdx}
		return label + f.renderChoices(yn, focused, false, width)
	default: // text / textarea / number
		if focused {
			return label + f.editorView()
		}
		val := strings.TrimSpace(fld.text)
		if val == "" {
			val = muted.Render("(empty)")
		} else {
			val = f.bg.Foreground(lipgloss.Color(f.theme.Text)).Render(oneLine(val, width-2))
		}
		return label + "  " + val
	}
}

// renderChoices draws an option list : a cursor caret on the focused row, a
// [x]/[ ] box for multi-select, the selected accent for single-select.
func (f *AskForm) renderChoices(fld *askField, focused, multi bool, width int) string {
	var lines []string
	for i, opt := range fld.options {
		onCursor := focused && i == fld.selIdx
		// Fixed columns so the marks stay aligned inside the block : a 2-cell
		// caret gutter ("› "/"  "), then the 4-cell checkbox (multi) or bullet
		// (single), then the label.
		gutter := "  "
		if onCursor {
			gutter = "› "
		}
		box := "  " // single-select : bullet only on the chosen row
		if multi {
			box = "[ ] "
			if fld.checked[i] {
				box = "[x] "
			}
		} else if i == fld.selIdx {
			box = "• "
		}
		row := gutter + box + opt

		switch {
		case onCursor:
			// Selection bar : a full-width highlight so the active row is
			// unmistakable. Pad to the content width so the bar spans edge-to-edge.
			if pad := width - lipgloss.Width(row); pad > 0 {
				row += strings.Repeat(" ", pad)
			}
			lines = append(lines, lipgloss.NewStyle().
				Background(lipgloss.Color(f.theme.BorderActive)).
				Foreground(lipgloss.Color(f.theme.Text)).
				Bold(true).
				Render(row))
		case (multi && fld.checked[i]) || (!multi && i == fld.selIdx):
			lines = append(lines, f.bg.Foreground(lipgloss.Color(f.theme.Accent)).Bold(true).Render(row))
		default:
			lines = append(lines, f.bg.Foreground(lipgloss.Color(f.theme.Text)).Render(row))
		}
	}
	return strings.Join(lines, "\n")
}

func (f *AskForm) editorView() string {
	return f.bg.
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(f.theme.BorderActive)).
		BorderBackground(lipgloss.Color(f.theme.BackgroundElement)).
		Render(f.ta.View())
}

func (f *AskForm) footer(muted lipgloss.Style) string {
	if f.submitting {
		return muted.Render("submitting …")
	}
	fld := f.fields[f.active]
	var hints []string
	switch fld.kind {
	case fieldSelect, fieldBoolean:
		hints = append(hints, "↑/↓ move", "enter select")
	case fieldMultiSelect:
		hints = append(hints, "↑/↓ move", "space toggle", "enter confirm")
	case fieldTextarea:
		hints = append(hints, "ctrl+d submit")
	default:
		hints = append(hints, "enter submit")
	}
	if len(f.fields) > 1 {
		hints = append([]string{"tab next"}, hints...)
		hints = append(hints, "ctrl+d submit")
	}
	hints = append(hints, "esc cancel")
	return muted.Render(strings.Join(hints, "   "))
}

// ---- small payload helpers (local to the form) ----------------------------

func stringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		switch s := e.(type) {
		case string:
			out = append(out, s)
		default:
			out = append(out, fmt.Sprintf("%v", e))
		}
	}
	return out
}

func mapSlice(v any) []map[string]any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return 0
}

func isNumericKey(k string) bool {
	if k == "." || k == "-" || k == "backspace" || k == "left" || k == "right" || k == "home" || k == "end" {
		return true
	}
	return len(k) == 1 && k[0] >= '0' && k[0] <= '9'
}

func isPrintableKey(km tea.KeyMsg) bool {
	return len(km.String()) == 1
}
