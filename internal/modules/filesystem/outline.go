package filesystem

import (
	"fmt"
	"regexp"
	"strings"
)

// outlineMaxEntries bounds an outline so a giant file can't flood the context.
const outlineMaxEntries = 500

// outlinePatterns match the lines that declare structure across the common
// languages — definitions (func/def/class/type/...), exported symbols, and
// markdown headings. This is a deliberately language-agnostic heuristic : it
// favours recall (show every plausible declaration with its line number) so a
// weak agent can map a 2000-line file in a few dozen lines, then jump straight
// to a line-range edit. It is NOT a parser — it never executes or rewrites.
var outlinePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^\s*(func|type|const|var)\s`),                                  // Go
	regexp.MustCompile(`^\s*(async\s+)?(def|class)\s`),                                 // Python / Ruby
	regexp.MustCompile(`^\s*(export\s+)?(default\s+)?(async\s+)?function[\s*]`),        // JS/TS function
	regexp.MustCompile(`^\s*(export\s+)?(default\s+)?(abstract\s+)?class\s`),           // JS/TS/Java class
	regexp.MustCompile(`^\s*(export\s+)?(interface|enum|struct|trait|impl|module)\s`),  // various decls
	regexp.MustCompile(`^\s*(export\s+)?(const|let|var)\s+\w+\s*=\s*(async\s*)?\(?[^=]*=>`), // JS arrow fn
	regexp.MustCompile(`^\s*(public|private|protected|internal|static|fn)\b.*\(`),      // Java/C#/Rust methods
	regexp.MustCompile(`^#{1,6}\s`),                                                    // markdown headings
}

func matchesOutline(line string) bool {
	for _, re := range outlinePatterns {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// outlineOf returns a cat -n style map of just the structural lines of content,
// plus how many it found. Empty result (no matches) means "no recognizable
// structure" — the caller should fall back to a normal numbered read.
func outlineOf(content string) (string, int) {
	lines := splitLines(content)
	var b strings.Builder
	width := len(fmt.Sprintf("%d", len(lines)))
	if width < 4 {
		width = 4
	}
	n := 0
	for i, l := range lines {
		if strings.TrimSpace(l) == "" || !matchesOutline(l) {
			continue
		}
		if n >= outlineMaxEntries {
			fmt.Fprintf(&b, "… (outline truncated at %d entries — read a line range for detail)\n", outlineMaxEntries)
			break
		}
		t := strings.TrimRight(l, " \t")
		if len(t) > 200 {
			t = t[:200] + " …"
		}
		fmt.Fprintf(&b, "%*d\t%s\n", width, i+1, t)
		n++
	}
	return b.String(), n
}
