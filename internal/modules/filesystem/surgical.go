package filesystem

import (
	"fmt"
	"strings"
)

// editLocator carries every way to point at an edit site. Exactly ONE locator
// mode must be set ; resolveEditOp validates that and dispatches to the matching
// strategy. The whole point is to let even a weak agent edit surgically WITHOUT
// reproducing exact text : it reads the numbered lines, then says "replace lines
// 12-15" or "insert after <anchor>". new_string is the content for every mode.
type editLocator struct {
	OldString    string // text locator : exact/fuzzy substring
	NewString    string // replacement / inserted content (all modes)
	ReplaceAll   bool   // text locator : change every match
	Occurrence   int    // text locator : change only the Nth match (1-based)
	StartLine    int    // line-range locator : 1-based inclusive
	EndLine      int    // line-range locator : 1-based inclusive (0 = same as start)
	InsertAfter  string // insert locator : after the unique line containing this
	InsertBefore string // insert locator : before the unique line containing this
	Prepend      bool   // insert at file start
	Append       bool   // insert at file end
	Expect       string // anti-drift : targeted text must contain this, else refuse
}

// lineModel splits content the SAME way read does (splitLines strips one
// trailing empty element), remembering whether the file ended with a newline so
// join() is byte-faithful. This guarantees start_line/end_line refer to exactly
// the 1-based line numbers the agent saw in read's cat -n output.
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

// replacementLines splits new_string into lines for a line-oriented insert/
// replace, stripping ONE structural trailing newline so the caller doesn't get
// a surprise blank line (the lines are re-joined with \n anyway). "" → no lines
// (a pure deletion).
func replacementLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// resolveEditOp picks the edit locator to apply and runs it, returning the new
// content (without touching disk), the count of changed units, and a strategy
// label.
//
// A weak agent often supplies MORE than one locator — classically old_string
// PLUS the start_line it read from a grep hit. Rather than rejecting (which
// just makes it fail), we resolve deterministically by PRECEDENCE and use the
// single most authoritative one :
//
//	old_string > insert_after > insert_before > line-range > prepend > append
//
// old_string leads because it is SELF-VALIDATING : a wrong or ambiguous match
// errors loudly ("not found" / "multiple matches") instead of silently editing
// the wrong place, whereas a positional line-range would corrupt silently if
// the agent miscounted. So preferring it is both the most specific intent AND
// the safest. The chosen strategy is returned in the result, so the resolution
// is never a black box.
func resolveEditOp(content string, loc editLocator) (updated string, count int, strategy string, err error) {
	switch {
	case loc.OldString != "":
		if loc.Occurrence > 0 {
			return editNthOccurrence(content, loc.OldString, loc.NewString, loc.Occurrence)
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

// editLineRange replaces lines [start, end] (1-based inclusive) with new_string.
// new_string="" deletes the range. With Expect set, the targeted lines must
// contain it or the edit is refused (the file changed since the agent read it).
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

// editInsert inserts new_string after/before the UNIQUE line containing anchor.
// A missing or non-unique anchor is refused with an actionable message.
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
	}
	ins := replacementLines(newStr)
	out := make([]string, 0, len(m.lines)+len(ins))
	out = append(out, m.lines[:at]...)
	out = append(out, ins...)
	out = append(out, m.lines[at:]...)
	m.lines = out
	return m.join(), 1, strat, nil
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
	case "exact", "line_range", "insert_after", "insert_before", "prepend", "append", "noop":
		return false
	}
	return !strings.HasPrefix(strategy, "occurrence")
}
