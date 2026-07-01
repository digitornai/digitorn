package render

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/digitornai/digitorn-cli/internal/theme"
)

// A wide code line (no spaces, like an ASCII diagram) must not produce a
// rendered line wider than the wrap width — glamour preserves code verbatim, so
// without wrapCodeFences it would overflow and the chat frame would garble it.
func TestMarkdown_WrapsWideCodeBlock(t *testing.T) {
	const width = 40
	wide := strings.Repeat("─", 200)
	md := "text before\n\n```\n" + wide + "\n```\n\ntext after"
	out := Markdown(md, width, theme.Default(), "")
	for _, ln := range strings.Split(out, "\n") {
		if w := ansi.StringWidth(ln); w > width {
			t.Fatalf("rendered line exceeds width %d (got %d): %q", width, w, ansi.Strip(ln))
		}
	}
}

// The bg argument must be BAKED into the rendered ANSI (glamour cascades a
// Document background to every element) so the panel fill rides inside the
// output — not patched on afterwards. #101010 → truecolor bg "48;2;16;16;16".
func TestMarkdown_BakesBackgroundIntoAnsi(t *testing.T) {
	out := Markdown("hello **world**", 40, theme.Default(), "#101010")
	if !strings.Contains(out, "48;2;16;16;16") {
		t.Fatalf("background not baked into rendered ANSI: %q", out)
	}
	// With no bg, no background escape is forced onto the document.
	plain := Markdown("hello **world**", 40, theme.Default(), "")
	if strings.Contains(plain, "48;2;16;16;16") {
		t.Fatalf("unexpected background escape when bg empty: %q", plain)
	}
}

func TestWrapCodeFences_LeavesProseAndShortCode(t *testing.T) {
	md := "a short paragraph that glamour will wrap itself\n\n```\nshort()\n```"
	if got := wrapCodeFences(md, 40); got != md {
		t.Fatalf("short content should be unchanged:\n%q", got)
	}
}
