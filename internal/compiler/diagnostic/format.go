package diagnostic

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mbathepaul/digitorn/internal/compiler/position"
)

type FormatOptions struct {
	Color        bool
	SourceLoader func(file string) ([]byte, error)
	ContextLines int
}

func DefaultFormatOptions() FormatOptions {
	return FormatOptions{Color: true, SourceLoader: os.ReadFile, ContextLines: 1}
}

func Format(w io.Writer, d Diagnostic, opts FormatOptions) {
	sev := colorize(d.Severity.String(), severityColor(d.Severity), opts.Color)
	code := colorize(string(d.Code), colorDim, opts.Color)
	fmt.Fprintf(w, "%s[%s]: %s\n", sev, code, d.Message)

	if !d.Pos.Zero() {
		fmt.Fprintf(w, "  %s %s\n", colorize("-->", colorDim, opts.Color), d.Pos)
	}
	if !d.Span.Zero() && opts.SourceLoader != nil {
		writeSnippet(w, d.Span, opts)
	}
	for _, h := range d.Hints {
		fmt.Fprintf(w, "   = %s %s\n", colorize("note:", colorDim, opts.Color), h)
	}
	if d.Suggestion != nil {
		reason := d.Suggestion.Reason
		if reason == "" {
			reason = fmt.Sprintf("did you mean %q?", d.Suggestion.Replacement)
		}
		fmt.Fprintf(w, "   = %s %s\n", colorize("help:", colorCyan, opts.Color), reason)
	}
	fmt.Fprintln(w)
}

func FormatBag(w io.Writer, b *Bag, opts FormatOptions) {
	for _, d := range b.Sorted() {
		Format(w, d, opts)
	}
	if n := len(b.Errors()); n > 0 {
		fmt.Fprintf(w, "%s\n", colorize(
			fmt.Sprintf("compilation failed: %d error(s)", n),
			colorRed, opts.Color,
		))
	}
}

func writeSnippet(w io.Writer, span position.Span, opts FormatOptions) {
	src, err := opts.SourceLoader(span.File)
	if err != nil {
		return
	}
	lines := strings.Split(string(src), "\n")
	if span.Start.Line < 1 || span.Start.Line > len(lines) {
		return
	}
	line := lines[span.Start.Line-1]
	gutter := fmt.Sprintf("%d", span.Start.Line)
	pad := strings.Repeat(" ", len(gutter))
	bar := colorize("|", colorDim, opts.Color)

	fmt.Fprintf(w, "%s %s\n", pad, bar)
	fmt.Fprintf(w, "%s %s %s\n", colorize(gutter, colorDim, opts.Color), bar, line)
	if span.Start.Line == span.End.Line {
		n := span.End.Column - span.Start.Column
		if n < 1 {
			n = 1
		}
		spaces := strings.Repeat(" ", span.Start.Column-1)
		caret := colorize(strings.Repeat("^", n), colorRed, opts.Color)
		fmt.Fprintf(w, "%s %s %s%s\n", pad, bar, spaces, caret)
	}
}

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
	colorDim    = "\033[2m"
)

func severityColor(s Severity) string {
	switch s {
	case SeverityError:
		return colorRed
	case SeverityWarning:
		return colorYellow
	case SeverityInfo:
		return colorCyan
	default:
		return colorDim
	}
}

func colorize(s, color string, enabled bool) string {
	if !enabled {
		return s
	}
	return color + s + colorReset
}
