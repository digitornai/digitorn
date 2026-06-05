package filesystem

import "testing"

// FuzzLocateAndApply hammers the edit matcher with arbitrary content/old_string
// pairs. The reliability contract it locks down: locateFuzzy never panics and
// always returns valid, in-bounds, ascending, NON-OVERLAPPING byte spans ;
// applyFuzzySpans never panics on them ; closestMatches never panics. A bad
// span here would mean a corrupt edit, so this is a core "ultra-fiable" guard.
func FuzzLocateAndApply(f *testing.F) {
	f.Add("hello\nworld\n", "world")
	f.Add("a\r\nb\r\nc\r\n", "a\nb")
	f.Add("\t\tx = 1\n\t\ty = 2\n", "x = 1\ny = 2")
	f.Add("dup  \nmid\ndup \n", "dup")
	f.Add("", "")
	f.Add("only one line no newline", "one line")
	f.Add("é€😀\nnext", "é€😀")

	f.Fuzz(func(t *testing.T, content, old string) {
		spans, strategy := locateFuzzy(content, old)

		prevEnd := 0
		for i, s := range spans {
			if s.start < 0 || s.end < s.start || s.end > len(content) {
				t.Fatalf("span %d %+v out of bounds for content len %d (strategy %q)", i, s, len(content), strategy)
			}
			if s.start < prevEnd {
				t.Fatalf("span %d %+v overlaps/precedes previous end %d", i, s, prevEnd)
			}
			prevEnd = s.end
		}

		// Applying the located spans must never panic and must yield a string
		// that is at least the untouched prefix + suffix length sane.
		if len(spans) > 0 {
			out := applyFuzzySpans(content, spans, "<<R>>")
			if len(out) < len(content)-totalSpan(spans) {
				t.Fatalf("apply lost content: out=%d content=%d spans=%v", len(out), len(content), spans)
			}
		}

		// Suggestions path must also never panic on arbitrary input.
		_ = closestMatches(content, old, 3)
	})
}

func totalSpan(spans []matchSpan) int {
	n := 0
	for _, s := range spans {
		n += s.end - s.start
	}
	return n
}
