// Package render exposes formatters that turn raw daemon payloads
// (markdown strings, tool calls, diffs) into ANSI-styled output ready
// to drop into a Bubble Tea viewport. Centralising rendering here
// means the TUI widgets stay layout-only.
package render

import (
	"strings"
	"sync"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"

	"github.com/mbathepaul/digitorn-cli/internal/theme"
)

// glamour.TermRenderer is expensive to build (it compiles the chroma
// formatter + the whole style config). Rebuilding one per call was fine for
// committed messages but catastrophic while streaming, where Markdown() was
// hit on every token over a growing buffer (O(n²) renderer construction).
// Cache one renderer per (width, theme) and reuse it, guarded by a mutex so a
// stray off-loop caller can't corrupt the shared renderer's internal buffer.
type rendererKey struct {
	width int
	theme *theme.Theme
	bg    string
}

var (
	rendererMu    sync.Mutex
	rendererCache = map[rendererKey]*glamour.TermRenderer{}
)

// Markdown renders a markdown string to ANSI-coloured terminal output
// sized for the given line width, themed with the supplied palette so
// headings/links/code/emphasis match the rest of the TUI (opencode
// style) instead of glamour's generic auto-style. Falls back to the
// raw string when glamour fails — a render bug must never blank the
// assistant's reply. width 0 falls back to 80 ; nil theme uses the
// default palette.
//
// bg (a hex colour, "" for none) is BAKED into the glamour style config so
// the panel background rides INSIDE the rendered ANSI — glamour propagates a
// Document background to every child element (opencode's approach). This
// replaces the old post-hoc "\x1b[0m"→bg surgery, which corrupted partial
// (streaming) output ; here the same renderer paints committed AND streaming
// markdown identically, so there is no raw-source-then-snap transition.
func Markdown(md string, width int, t *theme.Theme, bg string) string {
	if md == "" {
		return ""
	}
	if width <= 0 {
		width = 80
	}
	if t == nil {
		t = theme.Default()
	}
	rendererMu.Lock()
	defer rendererMu.Unlock()
	key := rendererKey{width: width, theme: t, bg: bg}
	r := rendererCache[key]
	if r == nil {
		var err error
		r, err = glamour.NewTermRenderer(
			glamour.WithStyles(markdownStyle(t, bg)),
			glamour.WithWordWrap(width),
			glamour.WithChromaFormatter("terminal16m"),
			glamour.WithEmoji(),
		)
		if err != nil {
			return md
		}
		rendererCache[key] = r
	}
	// Glamour preserves code blocks verbatim (no word-wrap), so a wide code line
	// — an ASCII diagram, a long command — overflows and the chat frame re-wraps
	// it, garbling the layout. Pre-wrap over-long lines INSIDE fences to fit
	// (width minus glamour's code indent+margin) so nothing downstream breaks.
	out, err := r.Render(wrapCodeFences(md, width-4))
	if err != nil {
		return md
	}
	return out
}

// wrapCodeFences hard-wraps lines INSIDE ``` code fences that exceed width
// (measured in runes — code is ~monospace, so close enough). Prose lines are
// left untouched for glamour's own word-wrap.
func wrapCodeFences(md string, width int) string {
	if width < 8 || !strings.Contains(md, "```") {
		return md
	}
	lines := strings.Split(md, "\n")
	out := make([]string, 0, len(lines))
	inFence := false
	for _, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			inFence = !inFence
			out = append(out, ln)
			continue
		}
		if !inFence {
			out = append(out, ln)
			continue
		}
		r := []rune(ln)
		for len(r) > width {
			out = append(out, string(r[:width]))
			r = r[width:]
		}
		out = append(out, string(r))
	}
	return strings.Join(out, "\n")
}

func sp(s string) *string { v := s; return &v }
func bp(v bool) *bool     { return &v }
func up(v uint) *uint     { return &v }

// markdownStyle builds a glamour ansi.StyleConfig from the theme. Each
// markdown element maps to a Markdown* colour ; code-block chroma
// tokens map to the Syntax* colours. When bg != "" it is set on the Document
// primitive ; glamour cascades a parent background to every child block
// (see ansi/style.go InheritFrom), so the whole reply paints on that surface
// with no per-element wiring and no post-render ANSI surgery.
func markdownStyle(t *theme.Theme, bg string) ansi.StyleConfig {
	var docBg *string
	if bg != "" {
		docBg = sp(bg)
	}
	return ansi.StyleConfig{
		Document: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{Color: sp(t.MarkdownText), BackgroundColor: docBg},
			Margin:         up(0),
		},
		BlockQuote: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{Color: sp(t.MarkdownBlockQuote), Italic: bp(true)},
			IndentToken:    sp("┃ "),
		},
		Paragraph: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: sp(t.MarkdownText)}},
		List: ansi.StyleList{
			StyleBlock:  ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: sp(t.MarkdownText)}},
			LevelIndent: 2,
		},
		// No "#"/"##" prefix : glamour would print it literally before the
		// styled title. Headings read as bold, colour-tinted text instead.
		Heading: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: sp(t.MarkdownHeading), Bold: bp(true)}},
		H1:      ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: sp(t.MarkdownHeading), Bold: bp(true), Underline: bp(true)}},
		H2:      ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: sp(t.MarkdownHeading), Bold: bp(true)}},
		H3:      ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: sp(t.MarkdownHeading), Bold: bp(true)}},
		H4:      ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: sp(t.MarkdownHeading), Bold: bp(true)}},
		H5:      ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: sp(t.MarkdownHeading), Bold: bp(true)}},
		H6:      ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: sp(t.MarkdownHeading), Bold: bp(true)}},
		Text:           ansi.StylePrimitive{Color: sp(t.MarkdownText)},
		Strikethrough:  ansi.StylePrimitive{CrossedOut: bp(true), Faint: bp(true)},
		Emph:           ansi.StylePrimitive{Color: sp(t.MarkdownEmph), Italic: bp(true)},
		Strong:         ansi.StylePrimitive{Color: sp(t.MarkdownStrong), Bold: bp(true)},
		HorizontalRule: ansi.StylePrimitive{Color: sp(t.MarkdownHorizontalRule), Format: "\n────────\n"},
		Item:           ansi.StylePrimitive{Color: sp(t.MarkdownListItem), BlockPrefix: "• "},
		Enumeration:    ansi.StylePrimitive{Color: sp(t.MarkdownListEnumeration), BlockPrefix: ". "},
		Task:           ansi.StyleTask{Ticked: "[✓] ", Unticked: "[ ] "},
		Link:           ansi.StylePrimitive{Color: sp(t.MarkdownLink), Underline: bp(true)},
		LinkText:       ansi.StylePrimitive{Color: sp(t.MarkdownLinkText), Bold: bp(true)},
		Image:          ansi.StylePrimitive{Color: sp(t.MarkdownImage), Underline: bp(true)},
		ImageText:      ansi.StylePrimitive{Color: sp(t.MarkdownImageText), Format: "🖼 {{.text}}"},
		Code: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{Color: sp(t.MarkdownCode)},
		},
		CodeBlock: ansi.StyleCodeBlock{
			StyleBlock: ansi.StyleBlock{
				StylePrimitive: ansi.StylePrimitive{Color: sp(t.MarkdownCodeBlock)},
				Margin:         up(1),
			},
			Chroma: &ansi.Chroma{
				Text:                ansi.StylePrimitive{Color: sp(t.MarkdownCodeBlock)},
				Comment:             ansi.StylePrimitive{Color: sp(t.SyntaxComment)},
				CommentPreproc:      ansi.StylePrimitive{Color: sp(t.SyntaxKeyword)},
				Keyword:             ansi.StylePrimitive{Color: sp(t.SyntaxKeyword)},
				KeywordReserved:     ansi.StylePrimitive{Color: sp(t.SyntaxKeyword)},
				KeywordNamespace:    ansi.StylePrimitive{Color: sp(t.SyntaxKeyword)},
				KeywordType:         ansi.StylePrimitive{Color: sp(t.SyntaxType)},
				Operator:            ansi.StylePrimitive{Color: sp(t.SyntaxOperator)},
				Punctuation:         ansi.StylePrimitive{Color: sp(t.SyntaxPunctuation)},
				Name:                ansi.StylePrimitive{Color: sp(t.SyntaxVariable)},
				NameBuiltin:         ansi.StylePrimitive{Color: sp(t.SyntaxType)},
				NameTag:             ansi.StylePrimitive{Color: sp(t.SyntaxKeyword)},
				NameAttribute:       ansi.StylePrimitive{Color: sp(t.SyntaxFunction)},
				NameClass:           ansi.StylePrimitive{Color: sp(t.SyntaxType)},
				NameConstant:        ansi.StylePrimitive{Color: sp(t.SyntaxNumber)},
				NameDecorator:       ansi.StylePrimitive{Color: sp(t.SyntaxFunction)},
				NameFunction:        ansi.StylePrimitive{Color: sp(t.SyntaxFunction)},
				LiteralNumber:       ansi.StylePrimitive{Color: sp(t.SyntaxNumber)},
				LiteralString:       ansi.StylePrimitive{Color: sp(t.SyntaxString)},
				LiteralStringEscape: ansi.StylePrimitive{Color: sp(t.SyntaxKeyword)},
				GenericDeleted:      ansi.StylePrimitive{Color: sp(t.DiffRemoved)},
				GenericInserted:     ansi.StylePrimitive{Color: sp(t.DiffAdded)},
				GenericEmph:         ansi.StylePrimitive{Italic: bp(true)},
				GenericStrong:       ansi.StylePrimitive{Bold: bp(true)},
			},
		},
		Table: ansi.StyleTable{
			StyleBlock:      ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: sp(t.MarkdownText)}},
			CenterSeparator: sp("┼"),
			ColumnSeparator: sp("│"),
			RowSeparator:    sp("─"),
		},
	}
}
