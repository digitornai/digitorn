// Package theme defines the colour palette shared by every TUI
// component. CLI-2 ships a single hardcoded dark theme ; CLI-8 adds
// JSON-file loading (one TOML file per theme, copied from
// opencode-v0.6.3) and hot-switching via Ctrl+,.
//
// Design : we mirror opencode's structure — every concept (status,
// markdown, diff, syntax) has its own dedicated colour field — so
// when CLI-8 ports the JSON loader, the field set is already there.
// We just don't use the diff/syntax fields yet ; they're reserved
// for RT-3 (tool result diffs) and code blocks in markdown.
//
// Storage : we keep colours as hex strings, then construct
// lipgloss.Color values at render time. lipgloss v2 changed
// lipgloss.Color from a type to a constructor function, so we can't
// hold "lipgloss.Color" as a field type. Strings are simpler anyway —
// CLI-8's JSON loader produces them directly.
package theme

// Theme is the single source of truth for colors. Every TUI widget
// must read colors from a Theme instance, never hardcode hex.
//
// Each field stores a hex string like "#fab283". Pass it through
// lipgloss.Color(...) when applying to a style.
type Theme struct {
	Name string

	// Background levels (Radix 1/2/3 convention).
	Background        string
	BackgroundPanel   string
	BackgroundElement string

	// Border levels (Radix 6/7/8).
	BorderSubtle string
	Border       string
	BorderActive string

	// Brand.
	Primary   string
	Secondary string
	Accent    string

	// Text.
	Text      string
	TextMuted string

	// Status.
	Error   string
	Warning string
	Success string
	Info    string

	// Phase badges (turn lifecycle). Mapped to Status colors at
	// render time but isolated as their own fields so themes can
	// over-ride them without affecting unrelated status uses.
	PhaseThinking   string
	PhaseToolUse    string
	PhasePersisting string
	PhaseDone       string

	// Markdown (themed glamour rendering of assistant replies).
	MarkdownText            string
	MarkdownHeading         string
	MarkdownLink            string
	MarkdownLinkText        string
	MarkdownCode            string
	MarkdownBlockQuote      string
	MarkdownEmph            string
	MarkdownStrong          string
	MarkdownHorizontalRule  string
	MarkdownListItem        string
	MarkdownListEnumeration string
	MarkdownImage           string
	MarkdownImageText       string
	MarkdownCodeBlock       string

	// Syntax highlighting (code blocks, diffs).
	SyntaxComment     string
	SyntaxKeyword     string
	SyntaxFunction    string
	SyntaxVariable    string
	SyntaxString      string
	SyntaxNumber      string
	SyntaxType        string
	SyntaxOperator    string
	SyntaxPunctuation string

	// Diff rendering (tool edits).
	DiffAdded            string
	DiffRemoved          string
	DiffContext          string
	DiffHunkHeader       string
	DiffHighlightAdded   string
	DiffHighlightRemoved string
	DiffAddedBg          string
	DiffRemovedBg        string
	DiffContextBg        string
	DiffLineNumber       string
}

// Default returns the built-in dark theme. Inspired by opencode's
// default (warm orange primary, blue secondary, purple accent) so
// users who know opencode find a familiar palette here.
func Default() *Theme {
	return &Theme{
		Name: "default-dark",

		Background:        "#0a0a0a",
		BackgroundPanel:   "#141414",
		BackgroundElement: "#1e1e1e",

		BorderSubtle: "#3c3c3c",
		Border:       "#484848",
		BorderActive: "#606060",

		Primary:   "#fab283", // warm orange (opencode brand)
		Secondary: "#5c9cf5", // blue
		Accent:    "#9d7cd8", // purple

		Text:      "#eeeeee",
		TextMuted: "#808080",

		Error:   "#e06c75",
		Warning: "#f5a742",
		Success: "#7fd88f",
		Info:    "#56b6c2",

		PhaseThinking:   "#56b6c2", // cyan
		PhaseToolUse:    "#9d7cd8", // purple
		PhasePersisting: "#f5a742", // orange
		PhaseDone:       "#7fd88f", // green

		MarkdownText:            "#eeeeee",
		MarkdownHeading:         "#9d7cd8",
		MarkdownLink:            "#fab283",
		MarkdownLinkText:        "#56b6c2",
		MarkdownCode:            "#7fd88f",
		MarkdownBlockQuote:      "#e5c07b",
		MarkdownEmph:            "#e5c07b",
		MarkdownStrong:          "#f5a742",
		MarkdownHorizontalRule:  "#808080",
		MarkdownListItem:        "#fab283",
		MarkdownListEnumeration: "#56b6c2",
		MarkdownImage:           "#fab283",
		MarkdownImageText:       "#56b6c2",
		MarkdownCodeBlock:       "#eeeeee",

		SyntaxComment:     "#808080",
		SyntaxKeyword:     "#9d7cd8",
		SyntaxFunction:    "#fab283",
		SyntaxVariable:    "#e06c75",
		SyntaxString:      "#7fd88f",
		SyntaxNumber:      "#f5a742",
		SyntaxType:        "#e5c07b",
		SyntaxOperator:    "#56b6c2",
		SyntaxPunctuation: "#eeeeee",

		DiffAdded:            "#4fd6be",
		DiffRemoved:          "#c53b53",
		DiffContext:          "#828bb8",
		DiffHunkHeader:       "#828bb8",
		DiffHighlightAdded:   "#b8db87",
		DiffHighlightRemoved: "#e26a75",
		DiffAddedBg:          "#20303b",
		DiffRemovedBg:        "#37222c",
		DiffContextBg:        "#141414",
		DiffLineNumber:       "#1e1e1e",
	}
}
