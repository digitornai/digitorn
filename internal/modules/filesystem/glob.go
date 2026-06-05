package filesystem

import (
	"strconv"
	"strings"
	"unicode"
)

// glob.go : a from-scratch matcher that goes well past the stdlib's path.Match.
// Brace expansion ({a,b}, nested, {1..9}/{a..z} ranges with an optional step),
// ** across path segments, *, ?, [...] with [!..]/[^..] negation, ranges and
// POSIX classes ([[:alpha:]] …), backslash escaping, and ksh-style extended
// globs ?(..) *(..) +(..) @(..) !(..). Shared by glob, grep --include and the
// .gitignore matcher, so every path-pattern surface gets the same reach.

const braceExpansionCap = 4096

func matchGlob(pattern, name string) bool {
	if !strings.ContainsRune(pattern, '{') {
		return matchSegs(strings.Split(pattern, "/"), strings.Split(name, "/"))
	}
	nameSegs := strings.Split(name, "/")
	for _, p := range expandBraces(pattern) {
		if matchSegs(strings.Split(p, "/"), nameSegs) {
			return true
		}
	}
	return false
}

func matchSegs(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			for len(rest) > 0 && rest[0] == "**" {
				rest = rest[1:]
			}
			if len(rest) == 0 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchSegs(rest, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if !segMatch([]rune(pat[0]), []rune(name[0])) {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// segMatch matches one path segment (no '/') against a fnmatch-style pattern.
func segMatch(p, s []rune) bool {
	for len(p) > 0 {
		switch c := p[0]; c {
		case '*':
			if len(p) > 1 && p[1] == '(' {
				return matchExt(p, s)
			}
			for len(p) > 1 && p[1] == '*' {
				p = p[1:]
			}
			if len(p) == 1 {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if segMatch(p[1:], s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(p) > 1 && p[1] == '(' {
				return matchExt(p, s)
			}
			if len(s) == 0 {
				return false
			}
			p, s = p[1:], s[1:]
		case '@', '+', '!':
			if len(p) > 1 && p[1] == '(' {
				return matchExt(p, s)
			}
			if len(s) == 0 || s[0] != c {
				return false
			}
			p, s = p[1:], s[1:]
		case '[':
			in, np, valid := matchClass(p, s)
			if !valid {
				if len(s) == 0 || s[0] != '[' {
					return false
				}
				p, s = p[1:], s[1:]
				continue
			}
			if len(s) == 0 || !in {
				return false
			}
			p, s = p[np:], s[1:]
		case '\\':
			if len(p) > 1 {
				if len(s) == 0 || s[0] != p[1] {
					return false
				}
				p, s = p[2:], s[1:]
				continue
			}
			if len(s) == 0 || s[0] != '\\' {
				return false
			}
			p, s = p[1:], s[1:]
		default:
			if len(s) == 0 || s[0] != c {
				return false
			}
			p, s = p[1:], s[1:]
		}
	}
	return len(s) == 0
}

// matchClass parses a [...] bracket expression at p[0] and reports whether s[0]
// is a member. valid is false for an unterminated class (caller treats '[' as a
// literal). np is the number of pattern runes the class spans.
func matchClass(p, s []rune) (in bool, np int, valid bool) {
	i := 1
	negate := false
	if i < len(p) && (p[i] == '!' || p[i] == '^') {
		negate = true
		i++
	}
	start := i
	type rng struct{ lo, hi rune }
	var ranges []rng
	var classes []func(rune) bool
	closed := false
	for i < len(p) {
		c := p[i]
		if c == ']' && i > start {
			i++
			closed = true
			break
		}
		if c == '[' && i+1 < len(p) && p[i+1] == ':' {
			j := i + 2
			for j < len(p) && p[j] != ':' {
				j++
			}
			if j+1 < len(p) && p[j] == ':' && p[j+1] == ']' {
				if fn := posixClass(string(p[i+2 : j])); fn != nil {
					classes = append(classes, fn)
					i = j + 2
					continue
				}
			}
		}
		if c == '\\' && i+1 < len(p) {
			c = p[i+1]
			i += 2
		} else {
			i++
		}
		if i+1 < len(p) && p[i] == '-' && p[i+1] != ']' {
			hi := p[i+1]
			if hi == '\\' && i+2 < len(p) {
				hi = p[i+2]
				i += 3
			} else {
				i += 2
			}
			ranges = append(ranges, rng{c, hi})
			continue
		}
		ranges = append(ranges, rng{c, c})
	}
	if !closed {
		return false, 0, false
	}
	if len(s) == 0 {
		return false, i, true
	}
	ch := s[0]
	for _, r := range ranges {
		if ch >= r.lo && ch <= r.hi {
			return !negate, i, true
		}
	}
	for _, fn := range classes {
		if fn(ch) {
			return !negate, i, true
		}
	}
	return negate, i, true
}

func posixClass(name string) func(rune) bool {
	switch name {
	case "alpha":
		return unicode.IsLetter
	case "digit":
		return unicode.IsDigit
	case "alnum":
		return func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
	case "space":
		return unicode.IsSpace
	case "upper":
		return unicode.IsUpper
	case "lower":
		return unicode.IsLower
	case "punct":
		return unicode.IsPunct
	case "blank":
		return func(r rune) bool { return r == ' ' || r == '\t' }
	case "cntrl":
		return unicode.IsControl
	case "print":
		return unicode.IsPrint
	case "graph":
		return func(r rune) bool { return unicode.IsGraphic(r) && !unicode.IsSpace(r) }
	case "xdigit":
		return func(r rune) bool {
			return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		}
	}
	return nil
}

// matchExt handles an extended-glob group X(alt|alt|…) where X is one of ?*+@!.
func matchExt(p, s []rune) bool {
	kind := p[0]
	depth, end := 0, -1
	for i := 1; i < len(p); i++ {
		switch p[i] {
		case '\\':
			i++
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		if len(s) == 0 || s[0] != p[0] {
			return false
		}
		return segMatch(p[1:], s[1:])
	}
	alts := splitAlts(p[2:end])
	rest := p[end+1:]
	switch kind {
	case '@':
		return extOnce(alts, rest, s)
	case '?':
		return segMatch(rest, s) || extOnce(alts, rest, s)
	case '*':
		return extStar(alts, rest, s)
	case '+':
		return extPlus(alts, rest, s)
	case '!':
		return extNeg(alts, rest, s)
	}
	return false
}

func extOnce(alts [][]rune, rest, s []rune) bool {
	for _, a := range alts {
		for i := 0; i <= len(s); i++ {
			if segMatch(a, s[:i]) && segMatch(rest, s[i:]) {
				return true
			}
		}
	}
	return false
}

func extStar(alts [][]rune, rest, s []rune) bool {
	if segMatch(rest, s) {
		return true
	}
	return extPlus(alts, rest, s)
}

func extPlus(alts [][]rune, rest, s []rune) bool {
	for _, a := range alts {
		for i := 1; i <= len(s); i++ {
			if segMatch(a, s[:i]) && extStar(alts, rest, s[i:]) {
				return true
			}
		}
	}
	return false
}

func extNeg(alts [][]rune, rest, s []rune) bool {
	for i := 0; i <= len(s); i++ {
		if !segMatch(rest, s[i:]) {
			continue
		}
		matchesAlt := false
		for _, a := range alts {
			if segMatch(a, s[:i]) {
				matchesAlt = true
				break
			}
		}
		if !matchesAlt {
			return true
		}
	}
	return false
}

func splitAlts(inner []rune) [][]rune {
	var alts [][]rune
	depth, start := 0, 0
	bracket := false
	for i := 0; i < len(inner); i++ {
		switch inner[i] {
		case '\\':
			i++
		case '[':
			bracket = true
		case ']':
			bracket = false
		case '(':
			if !bracket {
				depth++
			}
		case ')':
			if !bracket && depth > 0 {
				depth--
			}
		case '|':
			if depth == 0 && !bracket {
				alts = append(alts, inner[start:i])
				start = i + 1
			}
		}
	}
	return append(alts, inner[start:])
}

func expandBraces(pattern string) []string {
	out := expandBracesRec(pattern)
	if len(out) == 0 || len(out) > braceExpansionCap {
		return []string{pattern}
	}
	return out
}

func expandBracesRec(p string) []string {
	open := -1
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' {
			i++
			continue
		}
		if p[i] == '{' && matchingBrace(p, i) > i {
			open = i
			break
		}
	}
	if open < 0 {
		return []string{p}
	}
	closeAt := matchingBrace(p, open)
	prefix, inner, suffix := p[:open], p[open+1:closeAt], p[closeAt+1:]
	tails := expandBracesRec(suffix)

	options := splitTopLevelComma(inner)
	if len(options) <= 1 {
		if r := expandRange(inner); r != nil {
			options = r
		} else {
			res := make([]string, 0, len(tails))
			for _, t := range tails {
				res = append(res, prefix+"{"+inner+"}"+t)
			}
			return res
		}
	}

	var res []string
	for _, o := range options {
		for _, oe := range expandBracesRec(o) {
			for _, t := range tails {
				res = append(res, prefix+oe+t)
			}
		}
	}
	return res
}

func matchingBrace(p string, open int) int {
	depth := 0
	for i := open; i < len(p); i++ {
		switch p[i] {
		case '\\':
			i++
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return -1
}

func splitTopLevelComma(inner string) []string {
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(inner); i++ {
		switch inner[i] {
		case '\\':
			i++
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, inner[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, inner[start:])
}

func expandRange(inner string) []string {
	parts := strings.Split(inner, "..")
	if len(parts) != 2 && len(parts) != 3 {
		return nil
	}
	step := 1
	if len(parts) == 3 {
		v, err := strconv.Atoi(parts[2])
		if err != nil || v == 0 {
			return nil
		}
		step = v
		if step < 0 {
			step = -step
		}
	}
	lo, hi := parts[0], parts[1]
	if loN, err := strconv.Atoi(lo); err == nil {
		hiN, err2 := strconv.Atoi(hi)
		if err2 != nil {
			return nil
		}
		return rangeInts(loN, hiN, step, lo, hi)
	}
	loR, hiR := []rune(lo), []rune(hi)
	if len(loR) == 1 && len(hiR) == 1 && isAlpha(loR[0]) && isAlpha(hiR[0]) {
		return rangeRunes(loR[0], hiR[0], step)
	}
	return nil
}

func rangeInts(lo, hi, step int, loStr, hiStr string) []string {
	width := 0
	if padded(loStr) || padded(hiStr) {
		width = max(len(loStr), len(hiStr))
	}
	var out []string
	for v := lo; (lo <= hi && v <= hi) || (lo > hi && v >= hi); {
		out = append(out, formatPadded(v, width))
		if len(out) > braceExpansionCap {
			break
		}
		if lo <= hi {
			v += step
		} else {
			v -= step
		}
	}
	return out
}

func rangeRunes(lo, hi rune, step int) []string {
	var out []string
	for c := lo; (lo <= hi && c <= hi) || (lo > hi && c >= hi); {
		out = append(out, string(c))
		if len(out) > braceExpansionCap {
			break
		}
		if lo <= hi {
			c += rune(step)
		} else {
			c -= rune(step)
		}
	}
	return out
}

func padded(s string) bool {
	if len(s) > 1 && s[0] == '0' {
		return true
	}
	return len(s) > 2 && s[0] == '-' && s[1] == '0'
}

func formatPadded(v, width int) string {
	s := strconv.Itoa(v)
	if width <= 0 {
		return s
	}
	neg := ""
	if strings.HasPrefix(s, "-") {
		neg, s = "-", s[1:]
	}
	for len(neg)+len(s) < width {
		s = "0" + s
	}
	return neg + s
}

func isAlpha(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}
