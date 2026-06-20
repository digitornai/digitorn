package filesystem

import (
	"sort"
	"strings"
)

// edit_match.go : locate an edit's old_string inside file content, tolerating
// the whitespace/line-ending drift that makes an LLM's copy fail an exact
// match — without ever guessing WHERE to edit. Auto-applied matches are
// DETERMINISTIC (byte-identical modulo line endings, trailing spaces, or
// indentation) ; similarity is used ONLY to suggest closest matches on a total
// miss, never to apply a speculative edit. This is the deliberate divergence
// from the Python reference, which auto-applies a 0.85 block-similarity match
// and can silently edit the wrong region.

// matchSpan is a located occurrence of old_string as a byte range in content,
// plus the indentation delta to reapply to the replacement when the match was
// found indentation-agnostically ("" otherwise).
type matchSpan struct {
	start, end int
	indent     string
}

// locateFuzzy finds every occurrence of old inside content under the FIRST
// normalization strategy (line-endings → trailing-space → indentation) that
// yields at least one match, returning the byte spans and the strategy name.
// Empty result means no strategy matched. old is matched as whole lines, so it
// never splits a line mid-token (the safe, line-block model).
func locateFuzzy(content, old string) ([]matchSpan, string) {
	// Strategy order: least-lossy first. Each strategy is deterministic — a hit
	// means the file text equals old_string modulo exactly that normalization.
	type strategy struct {
		name   string
		norm   func(string) string
		indent bool // capture + reapply indentation delta
	}
	strategies := []strategy{
		{"line-endings", stripCR, false},
		{"trailing-space", func(s string) string { return strings.TrimRight(s, " \t\r") }, false},
		{"indentation", strings.TrimSpace, true},
	}
	for _, st := range strategies {
		if spans := matchLineBlocks(content, old, st.norm, st.indent); len(spans) > 0 {
			return spans, st.name
		}
	}
	return nil, ""
}

// matchLineBlocks slides old's logical lines over content's lines, comparing
// each line under norm. A window where every line matches yields a span over
// the EXACT original bytes of those content lines (never a char offset, which
// is where the Python port off-by-ones when the file lacks a trailing newline).
func matchLineBlocks(content, old string, norm func(string) string, captureIndent bool) []matchSpan {
	oldLines := splitLogicalLines(old)
	if len(oldLines) == 0 {
		return nil
	}
	lines, spans := splitLinesWithSpans(content)
	if len(oldLines) > len(lines) {
		return nil
	}
	normOld := make([]string, len(oldLines))
	for i, l := range oldLines {
		normOld[i] = norm(l)
	}
	var out []matchSpan
	for i := 0; i+len(oldLines) <= len(lines); i++ {
		ok := true
		for j := range oldLines {
			if norm(lines[i+j]) != normOld[j] {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		ms := matchSpan{start: spans[i].start, end: spans[i+len(oldLines)-1].end}
		if captureIndent {
			ms.indent = indentDelta(leadingWS(lines[i]), leadingWS(oldLines[0]))
		}
		out = append(out, ms)
		i += len(oldLines) - 1 // non-overlapping: resume after this block
	}
	return out
}

// reindentReplacement reapplies an indentation delta to every line of the
// replacement so a block matched at a different indent level lands correctly.
// A positive delta prepends whitespace ; a negative delta strips up to that
// many leading whitespace chars per line. Empty lines are left untouched.
func reindentReplacement(replacement, delta string) string {
	if delta == "" {
		return replacement
	}
	lines := strings.Split(replacement, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if strings.HasPrefix(delta, "-") {
			lines[i] = strings.TrimPrefix(l, delta[1:])
		} else {
			lines[i] = delta + l
		}
	}
	return strings.Join(lines, "\n")
}

// suggestion is a near-miss block surfaced to the agent when no strategy could
// locate old_string, so it can correct its edit instead of retrying blind.
type suggestion struct {
	StartLine  int     `json:"start_line"` // 1-based
	EndLine    int     `json:"end_line"`
	Similarity float64 `json:"similarity"` // 0..1
	Preview    string  `json:"preview"`
}

// closestMatches returns up to n windows of content (sized to old_string)
// ranked by line-similarity to old_string. Similarity is used ONLY here — for
// guidance — never to apply an edit.
func closestMatches(content, old string, n int) []suggestion {
	oldLines := splitLogicalLines(old)
	if len(oldLines) == 0 {
		return nil
	}
	lines, _ := splitLinesWithSpans(content)
	if len(lines) == 0 {
		return nil
	}
	win := len(oldLines)
	if win > len(lines) {
		win = len(lines)
	}
	oldJoined := strings.TrimSpace(strings.Join(oldLines, "\n"))
	var all []suggestion
	for i := 0; i+win <= len(lines); i++ {
		block := lines[i : i+win]
		sim := similarity(oldJoined, strings.TrimSpace(strings.Join(block, "\n")))
		if sim <= 0 {
			continue
		}
		all = append(all, suggestion{
			StartLine:  i + 1,
			EndLine:    i + win,
			Similarity: sim,
			// Full block content (not truncated) so the agent can use it directly
			// as old_string in a retry without a separate read call.
			Preview: strings.Join(block, "\n"),
		})
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].Similarity > all[b].Similarity })
	if len(all) > n {
		all = all[:n]
	}
	return all
}

// --- small helpers -------------------------------------------------------

func stripCR(s string) string { return strings.TrimRight(s, "\r") }

// stripReadLineNumbers removes the "  142\t" prefix that `read` prepends to every
// line (numberedSlice formats "%*d\t"), for when a model pastes numbered read
// output verbatim into old_string. Strips the prefix from each line that carries
// one and leaves other lines untouched — so a block where the model modified
// one line still resolves correctly. The prefix is only ever stripped when the
// exact old_string (with numbers) did NOT exist in the file (applyEditTry tried
// exact match first), so a line that genuinely starts with "42\tcontent" in the
// source file was already matched exactly and never reaches this function.
// Returns the input unchanged when no line had a number prefix.
func stripReadLineNumbers(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, len(lines))
	stripped := false
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			out[i] = l
			continue
		}
		rest, ok := trimLineNumberPrefix(l)
		if ok {
			out[i] = rest
			stripped = true
		} else {
			out[i] = l // keep the line as-is
		}
	}
	if !stripped {
		return s
	}
	return strings.Join(out, "\n")
}

// trimLineNumberPrefix strips a single leading "<spaces><digits>\t" from one line
// (the exact shape read emits). Reports whether a prefix was found and removed.
func trimLineNumberPrefix(l string) (string, bool) {
	i := 0
	for i < len(l) && l[i] == ' ' {
		i++
	}
	j := i
	for j < len(l) && l[j] >= '0' && l[j] <= '9' {
		j++
	}
	if j == i || j >= len(l) || l[j] != '\t' {
		return l, false
	}
	return l[j+1:], true
}

// splitLogicalLines splits on "\n" and drops a single trailing empty line so a
// trailing "\n" in old_string doesn't force matching a phantom blank line.
func splitLogicalLines(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "\n")
	if len(parts) > 1 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	for i := range parts {
		parts[i] = stripCR(parts[i])
	}
	return parts
}

type byteSpan struct{ start, end int }

// splitLinesWithSpans returns each line's text (without the trailing "\n", and
// with any "\r" kept so norm funcs can decide) and its [start,end) byte range
// in s (end excludes the newline). The trailing segment after the last "\n" is
// included as a final line so end-of-file blocks are matchable.
func splitLinesWithSpans(s string) ([]string, []byteSpan) {
	var lines []string
	var spans []byteSpan
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			spans = append(spans, byteSpan{start, i})
			start = i + 1
		}
	}
	lines = append(lines, s[start:])
	spans = append(spans, byteSpan{start, len(s)})
	return lines, spans
}

func leadingWS(s string) string {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return s[:i]
}

// indentDelta returns the prefix to ADD to replacement lines (have − want), or
// a "-"+prefix to STRIP when the file is less indented than old_string.
func indentDelta(have, want string) string {
	if have == want {
		return ""
	}
	if strings.HasPrefix(have, want) {
		return have[len(want):] // add the extra file indent
	}
	if strings.HasPrefix(want, have) {
		return "-" + want[len(have):] // strip old_string's extra indent
	}
	return "" // incomparable indents (tabs vs spaces mix) : leave replacement as-is
}

func clipPreview(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && (s[cut]&0xC0) == 0x80 { // back up off a UTF-8 continuation byte
		cut--
	}
	return s[:cut] + "…"
}

// similarity is a cheap 0..1 token-overlap ratio (Sørensen–Dice over whitespace
// tokens). Good enough to rank near-miss blocks ; not used to apply edits.
func similarity(a, b string) float64 {
	ta, tb := strings.Fields(a), strings.Fields(b)
	if len(ta) == 0 && len(tb) == 0 {
		return 1
	}
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	counts := make(map[string]int, len(tb))
	for _, t := range tb {
		counts[t]++
	}
	inter := 0
	for _, t := range ta {
		if counts[t] > 0 {
			counts[t]--
			inter++
		}
	}
	return 2 * float64(inter) / float64(len(ta)+len(tb))
}
