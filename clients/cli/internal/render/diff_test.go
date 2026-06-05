package render

import (
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn-cli/internal/theme"
)

const sampleDiff = `--- a/poc.txt
+++ b/poc.txt
@@ -1,3 +1,3 @@
 keep this line
-old answer
+new answer
 trailing line`

func TestDiffStat(t *testing.T) {
	if got := DiffStat(sampleDiff); got != "+1 -1" {
		t.Fatalf("DiffStat = %q, want %q", got, "+1 -1")
	}
	if got := DiffStat(""); got != "" {
		t.Fatalf("empty diff stat = %q, want empty", got)
	}
	if got := DiffStat("--- a\n+++ b\n@@ -0,0 +1 @@\n+only an add\n"); got != "+1" {
		t.Fatalf("add-only stat = %q, want %q", got, "+1")
	}
}

func TestDiff_RendersChangedLines(t *testing.T) {
	out := Diff(sampleDiff, 80, theme.Default())
	plain := stripSGR(out)
	// Content survives, with line numbers in the gutter.
	for _, want := range []string{"new answer", "old answer", "keep this line"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("diff render missing %q:\n%s", want, plain)
		}
	}
	// The unified-diff machinery is parsed away — never shown (opencode-style).
	for _, noise := range []string{"@@", "--- a/", "+++ b/"} {
		if strings.Contains(plain, noise) {
			t.Fatalf("diff render must not show %q (it's parsed, not printed):\n%s", noise, plain)
		}
	}
	if Diff("", 80, theme.Default()) != "" {
		t.Fatal("empty diff should render empty")
	}
}

func TestIntralineSpans_MarksOnlyChangedRuns(t *testing.T) {
	del, add := intralineSpans("the old answer", "the new answer")
	join := func(spans []intraSpan, changed bool) string {
		out := ""
		for _, s := range spans {
			if s.changed == changed {
				out += s.text
			}
		}
		return out
	}
	// "the " and " answer" are common (unchanged) on both sides.
	if got := join(del, false); got != "the  answer" {
		t.Fatalf("unchanged (old) = %q, want %q", got, "the  answer")
	}
	if got := join(add, false); got != "the  answer" {
		t.Fatalf("unchanged (new) = %q, want %q", got, "the  answer")
	}
	// The changed run is "old" on the old side, "new" on the new side.
	if got := join(del, true); got != "old" {
		t.Fatalf("changed (old) = %q, want %q", got, "old")
	}
	if got := join(add, true); got != "new" {
		t.Fatalf("changed (new) = %q, want %q", got, "new")
	}
}

func TestIntralineSpans_NoCommonRunsSkips(t *testing.T) {
	// Two totally different lines aren't an "edit" — no intra-line highlight.
	if del, add := intralineSpans("aaaa", "bbbb"); del != nil || add != nil {
		t.Fatalf("disjoint lines should not be intra-line highlighted: del=%v add=%v", del, add)
	}
}

func TestDiff_AnnotatesAdjacentEditPair(t *testing.T) {
	lines := parseUnified(sampleDiff)
	annotateIntraline(lines)
	var del, add *diffLine
	for i := range lines {
		if lines[i].kind == '-' {
			del = &lines[i]
		}
		if lines[i].kind == '+' {
			add = &lines[i]
		}
	}
	if del == nil || add == nil || len(del.segs) == 0 || len(add.segs) == 0 {
		t.Fatalf("the -/+ pair should be intra-line annotated: del=%+v add=%+v", del, add)
	}
}

func TestDiff_TruncatesLongLines(t *testing.T) {
	long := "--- a/x\n+++ b/x\n@@ -0,0 +1 @@\n+" + strings.Repeat("x", 200)
	out := stripSGR(Diff(long, 20, theme.Default()))
	for _, ln := range strings.Split(out, "\n") {
		if w := len([]rune(ln)); w > 20 {
			t.Fatalf("line not truncated to width: %d runes in %q", w, ln)
		}
	}
	if !strings.Contains(out, "…") {
		t.Fatalf("a truncated line should carry an ellipsis:\n%s", out)
	}
}

var sgrRe = func() func(string) string {
	return func(s string) string {
		var b strings.Builder
		inEsc := false
		for _, r := range s {
			if r == '\x1b' {
				inEsc = true
				continue
			}
			if inEsc {
				if r == 'm' {
					inEsc = false
				}
				continue
			}
			b.WriteRune(r)
		}
		return b.String()
	}
}()

func stripSGR(s string) string { return sgrRe(s) }
