package filesystem

import (
	"fmt"
	"strings"
)

type editLocator struct {
	OldString    string
	NewString    string
	ReplaceAll   bool
	Occurrence   int
	StartLine    int
	EndLine      int
	InsertAfter  string
	InsertBefore string
	Prepend      bool
	Append       bool
	Expect       string
}

type lineModel struct {
	lines      []string
	trailingNL bool
}

func newLineModel(content string) lineModel {
	return lineModel{lines: splitLines(content), trailingNL: strings.HasSuffix(content, "\n")}
}

func (m lineModel) join() string {
	s := strings.Join(m.lines, "\n")
	if m.trailingNL && s != "" {
		s += "\n"
	}
	return s
}

func replacementLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func resolveEditOp(content string, loc editLocator) (updated string, count int, strategy string, err error) {
	switch {
	case loc.OldString != "":
		if loc.Occurrence > 0 {
			return editNthOccurrence(content, loc.OldString, loc.NewString, loc.Occurrence)
		}
		if loc.StartLine > 0 && !loc.ReplaceAll {
			if upd, n, strat, ok := editNearestLine(content, loc.OldString, loc.NewString, loc.StartLine); ok {
				return upd, n, strat, nil
			}
		}
		return applyEdit(content, loc.OldString, loc.NewString, loc.ReplaceAll)
	case loc.InsertAfter != "":
		return editInsert(content, loc.InsertAfter, loc.NewString, true)
	case loc.InsertBefore != "":
		return editInsert(content, loc.InsertBefore, loc.NewString, false)
	case loc.StartLine > 0 || loc.EndLine > 0:
		return editLineRange(content, loc)
	case loc.Prepend:
		return editEnds(content, loc.NewString, true)
	case loc.Append:
		return editEnds(content, loc.NewString, false)
	default:
		return "", 0, "", &editError{kind: "args", message: "no edit locator: provide ONE of old_string, start_line(/end_line), insert_after, insert_before, prepend, or append"}
	}
}

func editLineRange(content string, loc editLocator) (string, int, string, error) {
	m := newLineModel(content)
	n := len(m.lines)
	start, end := loc.StartLine, loc.EndLine
	if start == 0 {
		start = end
	}
	if end == 0 {
		end = start
	}
	if start < 1 {
		return "", 0, "", &editError{kind: "range", message: fmt.Sprintf("start_line must be >= 1 (got %d)", start)}
	}
	if end < start {
		return "", 0, "", &editError{kind: "range", message: fmt.Sprintf("end_line (%d) must be >= start_line (%d) — you have them swapped. Correct call: edit(start_line=%d, end_line=%d, ...)", end, start, end, start)}
	}
	if start > n {
		return "", 0, "", &editError{kind: "range", message: fmt.Sprintf("start_line %d is past end of file (%d lines) — use append=true to add at the end", start, n)}
	}
	if end > n {
		end = n
	}
	if loc.Expect != "" {
		target := strings.Join(m.lines[start-1:end], "\n")
		if !strings.Contains(target, strings.TrimRight(loc.Expect, "\r\n")) {
			return "", 0, "", &editError{kind: "verify", message: fmt.Sprintf("expect mismatch: lines %d-%d do not contain the expected text (the file likely changed since you read it). Re-read and retry.", start, end)}
		}
	}
	repl := replacementLines(loc.NewString)
	out := make([]string, 0, n-(end-start+1)+len(repl))
	out = append(out, m.lines[:start-1]...)
	out = append(out, repl...)
	out = append(out, m.lines[end:]...)
	m.lines = out
	return m.join(), end - start + 1, "line_range", nil
}

func editInsert(content, anchor, newStr string, after bool) (string, int, string, error) {
	strat := "insert_before"
	if after {
		strat = "insert_after"
	}
	m := newLineModel(content)
	var hits []int
	for i, l := range m.lines {
		if strings.Contains(l, anchor) {
			hits = append(hits, i)
		}
	}
	if len(hits) == 0 {
		return "", 0, "", &editError{kind: "not_found", message: fmt.Sprintf("%s anchor %q not found on any line", strat, anchor), closest: closestMatches(content, anchor, 3)}
	}
	if len(hits) > 1 {
		ln := make([]int, 0, len(hits))
		for _, i := range hits {
			ln = append(ln, i+1)
		}
		return "", 0, "", &editError{kind: "ambiguous", message: fmt.Sprintf("%s anchor %q matches %d lines %v — include more of the line to make it unique", strat, anchor, len(hits), ln)}
	}
	at := hits[0]
	if after {
		at++
		// If the anchor line OPENS a brace block (function/class/if…), the agent
		// almost always means "after the whole block", not after the signature
		// line — inserting there would land inside the body. Jump past the
		// matching close brace. Falls back to line behaviour if braces don't
		// balance cleanly, so it's never worse than before.
		if end, ok := blockCloseLine(m.lines, hits[0]); ok && end+1 > at {
			at = end + 1
			strat = "insert_after_block"
		}
	}
	ins := replacementLines(newStr)
	out := make([]string, 0, len(m.lines)+len(ins))
	out = append(out, m.lines[:at]...)
	out = append(out, ins...)
	out = append(out, m.lines[at:]...)
	m.lines = out
	return m.join(), 1, strat, nil
}

// blockCloseLine returns the line index of the "}" that closes the first "{"
// opened on/after line start, or ok=false if the anchor line opens no block or
// the braces don't balance before EOF. Skips braces inside strings and //, /* */
// comments so code text can't throw off the count.
func blockCloseLine(lines []string, start int) (int, bool) {
	depth := 0
	opened := false
	var str byte // 0 or the open quote: " ' `
	inBlock := false
	for li := start; li < len(lines); li++ {
		l := lines[li]
		lineComment := false
		for i := 0; i < len(l); i++ {
			c := l[i]
			switch {
			case inBlock:
				if c == '*' && i+1 < len(l) && l[i+1] == '/' {
					inBlock = false
					i++
				}
			case lineComment:
				i = len(l)
			case str != 0:
				if c == '\\' {
					i++
				} else if c == str {
					str = 0
				}
			case c == '/' && i+1 < len(l) && l[i+1] == '/':
				lineComment = true
			case c == '/' && i+1 < len(l) && l[i+1] == '*':
				inBlock = true
				i++
			case c == '"' || c == '\'' || c == '`':
				str = c
			case c == '{':
				depth++
				opened = true
			case c == '}':
				depth--
				if opened && depth == 0 {
					return li, true
				}
				if depth < 0 {
					return 0, false
				}
			}
		}
	}
	return 0, false
}

// editEnds prepends or appends new_string as whole lines, preserving the file's
// trailing-newline convention.
func editEnds(content, newStr string, prepend bool) (string, int, string, error) {
	m := newLineModel(content)
	ins := replacementLines(newStr)
	if len(ins) == 0 {
		return content, 0, "noop", nil
	}
	if prepend {
		m.lines = append(append([]string{}, ins...), m.lines...)
		return m.join(), 1, "prepend", nil
	}
	m.lines = append(m.lines, ins...)
	return m.join(), 1, "append", nil
}

// editNearestLine disambiguates an old_string that occurs several times by the
// start_line the agent read: among the EXACT matches, it edits the one whose
// line is closest to the hint. Only a tiebreak between identical matches (never
// a speculative edit), so it can't corrupt. ok=false when old_string is unique
// (0 or 1 match) so the normal path handles it (and its miss/closest hint).
func editNearestLine(content, oldStr, newStr string, line int) (string, int, string, bool) {
	if oldStr == "" {
		return "", 0, "", false
	}
	var offs []int
	for off := 0; ; {
		i := strings.Index(content[off:], oldStr)
		if i < 0 {
			break
		}
		abs := off + i
		offs = append(offs, abs)
		off = abs + len(oldStr)
	}
	if len(offs) < 2 {
		return "", 0, "", false
	}
	best, bestDist := 0, 1<<30
	for idx, abs := range offs {
		ln := 1 + strings.Count(content[:abs], "\n")
		d := ln - line
		if d < 0 {
			d = -d
		}
		if d < bestDist {
			bestDist, best = d, idx
		}
	}
	abs := offs[best]
	return content[:abs] + newStr + content[abs+len(oldStr):], 1, "old_string@nearest_line", true
}

// editNthOccurrence replaces ONLY the Nth (1-based) exact occurrence of oldStr,
// so the agent can target one of several identical spots without reproducing
// surrounding context.
func editNthOccurrence(content, oldStr, newStr string, n int) (string, int, string, error) {
	if oldStr == "" {
		return "", 0, "", &editError{kind: "empty", message: "old_string must not be empty"}
	}
	var offs []int
	for off := 0; ; {
		i := strings.Index(content[off:], oldStr)
		if i < 0 {
			break
		}
		abs := off + i
		offs = append(offs, abs)
		off = abs + len(oldStr)
	}
	if len(offs) == 0 {
		return "", 0, "", &editError{kind: "not_found", message: "old_string not found", closest: closestMatches(content, oldStr, 3)}
	}
	if n > len(offs) {
		return "", 0, "", &editError{kind: "range", message: fmt.Sprintf("occurrence %d out of range — old_string has only %d match(es)", n, len(offs))}
	}
	abs := offs[n-1]
	return content[:abs] + newStr + content[abs+len(oldStr):], 1, fmt.Sprintf("occurrence#%d", n), nil
}

// isFuzzyStrategy reports whether a strategy label came from the forgiving text
// matcher (vs a precise positional locator) — surfaced as the "fuzzy" result
// flag so the agent knows when its match wasn't byte-exact.
func isFuzzyStrategy(strategy string) bool {
	switch strategy {
	case "exact", "line_range", "insert_after", "insert_after_block", "insert_before", "prepend", "append", "noop":
		return false
	}
	return !strings.HasPrefix(strategy, "occurrence")
}
