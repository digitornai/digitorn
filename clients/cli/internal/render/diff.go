package render

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/mbathepaul/digitorn-cli/internal/theme"
)

// diffLine is one parsed unified-diff line, annotated with its 1-based line
// numbers in the old/new file (0 = absent on that side). segs is set only for a
// removed/added line paired with its counterpart, marking which runs of text
// actually changed (intra-line highlight) ; nil means highlight the whole line.
type diffLine struct {
	kind         byte // '+', '-', ' '
	oldNo, newNo int
	text         string
	segs         []intraSpan
}

// intraSpan is a run of a line's text flagged as changed or unchanged, for
// character-level highlighting of an edited line.
type intraSpan struct {
	text    string
	changed bool
}

// Diff renders a unified diff the way opencode / Claude Code do : a line-number
// gutter, then each line marked +/-/space with a full-width background tint —
// NOT the raw "--- a/…", "+++ b/…" and "@@" machinery, which is parsed for line
// numbers and then dropped. Lines are truncated to width so a long one never
// wraps and breaks the chip layout. Falls back to the default theme when nil.
func Diff(unified string, width int, t *theme.Theme) string {
	if strings.TrimSpace(unified) == "" {
		return ""
	}
	if t == nil {
		t = theme.Default()
	}
	if width < 8 {
		width = 8
	}
	lines := parseUnified(unified)
	if len(lines) == 0 {
		return ""
	}
	annotateIntraline(lines)

	maxNo := 0
	for _, l := range lines {
		if l.newNo > maxNo {
			maxNo = l.newNo
		}
		if l.oldNo > maxNo {
			maxNo = l.oldNo
		}
	}
	numW := len(itoa(maxNo))
	if numW < 2 {
		numW = 2
	}
	gutterW := numW + 2 // " <num> "
	contentW := width - gutterW
	if contentW < 6 {
		contentW = 6
	}

	pick := func(c, fallback string) string {
		if c == "" {
			return fallback
		}
		return c
	}
	gutterStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color(pick(t.DiffContext, t.TextMuted))).
		Background(lipgloss.Color(pick(t.DiffLineNumber, t.BackgroundPanel)))

	var b strings.Builder
	for i, l := range lines {
		var num, lineBg, markerFg string
		marker := " "
		switch l.kind {
		case '+':
			num, marker = itoa(l.newNo), "+"
			lineBg, markerFg = pick(t.DiffAddedBg, ""), pick(t.DiffAdded, t.Success)
		case '-':
			num, marker = itoa(l.oldNo), "-"
			lineBg, markerFg = pick(t.DiffRemovedBg, ""), pick(t.DiffRemoved, t.Error)
		default:
			num = itoa(l.newNo)
			lineBg, markerFg = pick(t.DiffContextBg, ""), pick(t.DiffContext, t.TextMuted)
		}

		gutter := gutterStyle.Render(" " + padLeft(num, numW) + " ")

		markerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(markerFg))
		textStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(t.Text))
		rowStyle := lipgloss.NewStyle().Width(contentW)
		// Intra-line highlight : the runs that actually changed get a brighter
		// bar (dark text on the highlight colour), the rest keeps the line tint.
		var hlStyle lipgloss.Style
		if lineBg != "" {
			bg := lipgloss.Color(lineBg)
			markerStyle, textStyle, rowStyle = markerStyle.Background(bg), textStyle.Background(bg), rowStyle.Background(bg)
		}
		if l.kind == '+' {
			hlStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(pick(t.Background, t.Text))).Background(lipgloss.Color(pick(t.DiffHighlightAdded, pick(t.DiffAddedBg, t.DiffAdded))))
		} else if l.kind == '-' {
			hlStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(pick(t.Background, t.Text))).Background(lipgloss.Color(pick(t.DiffHighlightRemoved, pick(t.DiffRemovedBg, t.DiffRemoved))))
		}

		// Diff text stays NEUTRAL : the add/remove signal is the line
		// BACKGROUND tint (+ the coloured marker), never the foreground. Syntax-
		// colouring the content fought the tint — e.g. aura paints strings green
		// on a green added-bg, so the text vanished. Only the intra-line changed
		// runs get a brighter background bar (still neutral/dark text).
		body := markerStyle.Render(marker+" ") + renderLineText(l, textStyle, hlStyle, contentW-2)
		b.WriteString(gutter + rowStyle.Render(body))
		if i < len(lines)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// renderLineText renders a line's text to maxW visible runes : plain (base
// style) when there's no intra-line annotation, or per-span when there is —
// changed runs in hl, unchanged runs in base. Truncates with an ellipsis.
func renderLineText(l diffLine, base, hl lipgloss.Style, maxW int) string {
	if maxW <= 0 {
		return ""
	}
	if len(l.segs) == 0 {
		return base.Render(truncateRunes(l.text, maxW))
	}
	var b strings.Builder
	used := 0
	for _, sp := range l.segs {
		if used >= maxW {
			break
		}
		r := []rune(sp.text)
		if used+len(r) > maxW {
			r = r[:maxW-used-1]
			style := base
			if sp.changed {
				style = hl
			}
			b.WriteString(style.Render(string(r) + "…"))
			used = maxW
			break
		}
		style := base
		if sp.changed {
			style = hl
		}
		b.WriteString(style.Render(sp.text))
		used += len(r)
	}
	return b.String()
}

// annotateIntraline fills segs for each removed line immediately followed by an
// added line — the common shape of an edited line — so the renderer can mark
// exactly which characters changed instead of repainting the whole line. Mirrors
// opencode's HighlightIntralineChanges (which pairs i with i+1).
func annotateIntraline(lines []diffLine) {
	for i := 0; i+1 < len(lines); i++ {
		if lines[i].kind != '-' || lines[i+1].kind != '+' {
			continue
		}
		del, add := intralineSpans(lines[i].text, lines[i+1].text)
		lines[i].segs = del
		lines[i+1].segs = add
		i++ // consume the pair
	}
}

// intralineMaxRunes caps the O(n·m) rune LCS so a pair of very long lines can't
// stall the render ; past it we leave segs nil (whole-line highlight).
const intralineMaxRunes = 600

// intralineSpans diffs two line contents at the rune level and returns the
// change spans for the old (deletions) and new (insertions) sides. Adjacent
// runs of the same changed-ness are merged so the highlight reads as words, not
// scattered characters.
func intralineSpans(oldStr, newStr string) (del, add []intraSpan) {
	a, bb := []rune(oldStr), []rune(newStr)
	if len(a) > intralineMaxRunes || len(bb) > intralineMaxRunes {
		return nil, nil
	}
	ops := runeDiff(a, bb)
	for _, op := range ops {
		switch op.kind {
		case '=':
			del = appendSpan(del, string(op.r), false)
			add = appendSpan(add, string(op.r), false)
		case '-':
			del = appendSpan(del, string(op.r), true)
		case '+':
			add = appendSpan(add, string(op.r), true)
		}
	}
	// A pair with no common runs at all isn't really an "edit" — skip the
	// highlight so it doesn't just paint both whole lines.
	if !hasUnchanged(del) && !hasUnchanged(add) {
		return nil, nil
	}
	return del, add
}

func appendSpan(spans []intraSpan, r string, changed bool) []intraSpan {
	if n := len(spans); n > 0 && spans[n-1].changed == changed {
		spans[n-1].text += r
		return spans
	}
	return append(spans, intraSpan{text: r, changed: changed})
}

func hasUnchanged(spans []intraSpan) bool {
	for _, s := range spans {
		if !s.changed && s.text != "" {
			return true
		}
	}
	return false
}

// runeOp is one rune-level edit operation.
type runeOp struct {
	kind byte // '=', '-', '+'
	r    rune
}

// runeDiff is a classic LCS edit script over runes (same shape as the daemon's
// line-level lcsOps, one level down). Memory is O(len(a)·len(b)), bounded by the
// caller at intralineMaxRunes.
func runeDiff(a, b []rune) []runeOp {
	na, nb := len(a), len(b)
	dp := make([][]int, na+1)
	for i := range dp {
		dp[i] = make([]int, nb+1)
	}
	for i := na - 1; i >= 0; i-- {
		for j := nb - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	ops := make([]runeOp, 0, na+nb)
	i, j := 0, 0
	for i < na && j < nb {
		switch {
		case a[i] == b[j]:
			ops = append(ops, runeOp{'=', a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, runeOp{'-', a[i]})
			i++
		default:
			ops = append(ops, runeOp{'+', b[j]})
			j++
		}
	}
	for ; i < na; i++ {
		ops = append(ops, runeOp{'-', a[i]})
	}
	for ; j < nb; j++ {
		ops = append(ops, runeOp{'+', b[j]})
	}
	return ops
}

// parseUnified turns a unified diff into rendered lines, tracking 1-based line
// numbers from the @@ hunk headers and discarding the ---/+++/@@/"No newline"
// noise (opencode does the same in ParseUnifiedDiff).
func parseUnified(u string) []diffLine {
	var out []diffLine
	var oldNo, newNo int
	inBody := false
	for _, ln := range strings.Split(strings.TrimRight(u, "\n"), "\n") {
		switch {
		case strings.HasPrefix(ln, "--- "), strings.HasPrefix(ln, "+++ "):
			continue
		case strings.HasPrefix(ln, "@@"):
			oldNo, newNo = parseHunkHeader(ln)
			inBody = true
			continue
		case strings.HasPrefix(ln, `\ No newline`):
			continue
		}
		if !inBody {
			continue
		}
		if ln == "" {
			out = append(out, diffLine{kind: ' ', oldNo: oldNo, newNo: newNo})
			oldNo++
			newNo++
			continue
		}
		switch ln[0] {
		case '+':
			out = append(out, diffLine{kind: '+', newNo: newNo, text: ln[1:]})
			newNo++
		case '-':
			out = append(out, diffLine{kind: '-', oldNo: oldNo, text: ln[1:]})
			oldNo++
		default:
			out = append(out, diffLine{kind: ' ', oldNo: oldNo, newNo: newNo, text: strings.TrimPrefix(ln, " ")})
			oldNo++
			newNo++
		}
	}
	return out
}

// parseHunkHeader reads the starting old/new line numbers from "@@ -a,b +c,d @@".
func parseHunkHeader(ln string) (oldStart, newStart int) {
	parts := strings.Split(ln, " ")
	if len(parts) < 3 {
		return 1, 1
	}
	old := strings.TrimPrefix(parts[1], "-")
	nw := strings.TrimPrefix(parts[2], "+")
	if i := strings.IndexByte(old, ','); i >= 0 {
		old = old[:i]
	}
	if i := strings.IndexByte(nw, ','); i >= 0 {
		nw = nw[:i]
	}
	oldStart = atoi(old)
	newStart = atoi(nw)
	if oldStart == 0 {
		oldStart = 1
	}
	if newStart == 0 {
		newStart = 1
	}
	return oldStart, newStart
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func padLeft(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return strings.Repeat(" ", w-len(s)) + s
}

// DiffStat counts the changed lines in a unified diff as "+A -D", ignoring the
// ---/+++ file headers. Empty when the diff carries no changes.
func DiffStat(unified string) string {
	adds, dels := 0, 0
	for _, ln := range strings.Split(unified, "\n") {
		switch {
		case strings.HasPrefix(ln, "+++"), strings.HasPrefix(ln, "---"):
			continue
		case strings.HasPrefix(ln, "+"):
			adds++
		case strings.HasPrefix(ln, "-"):
			dels++
		}
	}
	if adds == 0 && dels == 0 {
		return ""
	}
	out := ""
	if adds > 0 {
		out = "+" + itoa(adds)
	}
	if dels > 0 {
		if out != "" {
			out += " "
		}
		out += "-" + itoa(dels)
	}
	return out
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
