package theme

import (
	"embed"
	"encoding/json"
	"sort"
	"strings"
)

// themesFS embeds the opencode theme JSON files (copied verbatim from
// opencode v0.6.3). Each is parsed into a *Theme at startup so the TUI
// can switch palettes live.
//
//go:embed themes/*.json
var themesFS embed.FS

type jsonTheme struct {
	Defs  map[string]any `json:"defs"`
	Theme map[string]any `json:"theme"`
}

var (
	registry   = map[string]*Theme{}
	themeNames []string
)

func init() { loadEmbedded() }

// Names returns the sorted list of available theme names.
func Names() []string { return themeNames }

// Get returns the theme registered under name, or nil if unknown.
func Get(name string) *Theme {
	if t, ok := registry[name]; ok {
		clone := *t
		return &clone
	}
	return nil
}

func loadEmbedded() {
	entries, err := themesFS.ReadDir("themes")
	if err != nil {
		return
	}
	for _, e := range entries {
		b, err := themesFS.ReadFile("themes/" + e.Name())
		if err != nil {
			continue
		}
		var jt jsonTheme
		if json.Unmarshal(b, &jt) != nil {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		registry[name] = fromJSON(name, jt)
		themeNames = append(themeNames, name)
	}
	sort.Strings(themeNames)
}

// resolveColor turns a theme value (a def-reference, a {dark,light}
// object, or a literal) into a usable colour string, preferring dark.
func resolveColor(defs map[string]any, v any) string {
	switch x := v.(type) {
	case string:
		if d, ok := defs[x]; ok {
			if ds, ok := d.(string); ok {
				return ds
			}
		}
		return x
	case map[string]any:
		if dk, ok := x["dark"]; ok {
			return resolveColor(defs, dk)
		}
		if lt, ok := x["light"]; ok {
			return resolveColor(defs, lt)
		}
	}
	return ""
}

// fromJSON maps an opencode JSON theme onto our Theme struct. Missing
// fields fall back to the built-in default so a partial theme never
// renders blank colours.
func fromJSON(name string, jt jsonTheme) *Theme {
	get := func(key string) string {
		v, ok := jt.Theme[key]
		if !ok {
			return ""
		}
		c := resolveColor(jt.Defs, v)
		if c == "none" {
			return ""
		}
		return c
	}
	t := &Theme{
		Name:              name,
		Background:        get("background"),
		BackgroundPanel:   get("backgroundPanel"),
		BackgroundElement: get("backgroundElement"),
		BorderSubtle:      get("borderSubtle"),
		Border:            get("border"),
		BorderActive:      get("borderActive"),
		Primary:           get("primary"),
		Secondary:         get("secondary"),
		Accent:            get("accent"),
		Text:              get("text"),
		TextMuted:         get("textMuted"),
		Error:             get("error"),
		Warning:           get("warning"),
		Success:           get("success"),
		Info:              get("info"),

		MarkdownText:            get("markdownText"),
		MarkdownHeading:         get("markdownHeading"),
		MarkdownLink:            get("markdownLink"),
		MarkdownLinkText:        get("markdownLinkText"),
		MarkdownCode:            get("markdownCode"),
		MarkdownBlockQuote:      get("markdownBlockQuote"),
		MarkdownEmph:            get("markdownEmph"),
		MarkdownStrong:          get("markdownStrong"),
		MarkdownHorizontalRule:  get("markdownHorizontalRule"),
		MarkdownListItem:        get("markdownListItem"),
		MarkdownListEnumeration: get("markdownListEnumeration"),
		MarkdownImage:           get("markdownImage"),
		MarkdownImageText:       get("markdownImageText"),
		MarkdownCodeBlock:       get("markdownCodeBlock"),

		SyntaxComment:     get("syntaxComment"),
		SyntaxKeyword:     get("syntaxKeyword"),
		SyntaxFunction:    get("syntaxFunction"),
		SyntaxVariable:    get("syntaxVariable"),
		SyntaxString:      get("syntaxString"),
		SyntaxNumber:      get("syntaxNumber"),
		SyntaxType:        get("syntaxType"),
		SyntaxOperator:    get("syntaxOperator"),
		SyntaxPunctuation: get("syntaxPunctuation"),

		DiffAdded:            get("diffAdded"),
		DiffRemoved:          get("diffRemoved"),
		DiffContext:          get("diffContext"),
		DiffHunkHeader:       get("diffHunkHeader"),
		DiffHighlightAdded:   get("diffHighlightAdded"),
		DiffHighlightRemoved: get("diffHighlightRemoved"),
		DiffAddedBg:          get("diffAddedBg"),
		DiffRemovedBg:        get("diffRemovedBg"),
		DiffContextBg:        get("diffContextBg"),
		DiffLineNumber:       get("diffLineNumber"),
	}
	// Phase badges aren't in the opencode schema — derive from status.
	t.PhaseThinking = t.Info
	t.PhaseToolUse = t.Accent
	t.PhasePersisting = t.Warning
	t.PhaseDone = t.Success

	t.fillFrom(Default())
	return t
}

// fillFrom copies any empty colour field from src, so a theme missing a
// key never renders an empty (broken) colour.
func (t *Theme) fillFrom(src *Theme) {
	if t.Background == "" {
		t.Background = src.Background
	}
	if t.BackgroundPanel == "" {
		t.BackgroundPanel = src.BackgroundPanel
	}
	if t.BackgroundElement == "" {
		t.BackgroundElement = src.BackgroundElement
	}
	if t.BorderSubtle == "" {
		t.BorderSubtle = src.BorderSubtle
	}
	if t.Border == "" {
		t.Border = src.Border
	}
	if t.BorderActive == "" {
		t.BorderActive = src.BorderActive
	}
	if t.Primary == "" {
		t.Primary = src.Primary
	}
	if t.Secondary == "" {
		t.Secondary = src.Secondary
	}
	if t.Accent == "" {
		t.Accent = src.Accent
	}
	if t.Text == "" {
		t.Text = src.Text
	}
	if t.TextMuted == "" {
		t.TextMuted = src.TextMuted
	}
	if t.Error == "" {
		t.Error = src.Error
	}
	if t.Warning == "" {
		t.Warning = src.Warning
	}
	if t.Success == "" {
		t.Success = src.Success
	}
	if t.Info == "" {
		t.Info = src.Info
	}
	for _, p := range []struct {
		dst *string
		src string
	}{
		{&t.MarkdownText, src.MarkdownText}, {&t.MarkdownHeading, src.MarkdownHeading},
		{&t.MarkdownLink, src.MarkdownLink}, {&t.MarkdownLinkText, src.MarkdownLinkText},
		{&t.MarkdownCode, src.MarkdownCode}, {&t.MarkdownBlockQuote, src.MarkdownBlockQuote},
		{&t.MarkdownEmph, src.MarkdownEmph}, {&t.MarkdownStrong, src.MarkdownStrong},
		{&t.MarkdownHorizontalRule, src.MarkdownHorizontalRule}, {&t.MarkdownListItem, src.MarkdownListItem},
		{&t.MarkdownListEnumeration, src.MarkdownListEnumeration}, {&t.MarkdownImage, src.MarkdownImage},
		{&t.MarkdownImageText, src.MarkdownImageText}, {&t.MarkdownCodeBlock, src.MarkdownCodeBlock},
		{&t.SyntaxComment, src.SyntaxComment}, {&t.SyntaxKeyword, src.SyntaxKeyword},
		{&t.SyntaxFunction, src.SyntaxFunction}, {&t.SyntaxVariable, src.SyntaxVariable},
		{&t.SyntaxString, src.SyntaxString}, {&t.SyntaxNumber, src.SyntaxNumber},
		{&t.SyntaxType, src.SyntaxType}, {&t.SyntaxOperator, src.SyntaxOperator},
		{&t.SyntaxPunctuation, src.SyntaxPunctuation},
		{&t.DiffAdded, src.DiffAdded}, {&t.DiffRemoved, src.DiffRemoved},
		{&t.DiffContext, src.DiffContext}, {&t.DiffHunkHeader, src.DiffHunkHeader},
		{&t.DiffHighlightAdded, src.DiffHighlightAdded}, {&t.DiffHighlightRemoved, src.DiffHighlightRemoved},
		{&t.DiffAddedBg, src.DiffAddedBg}, {&t.DiffRemovedBg, src.DiffRemovedBg},
		{&t.DiffContextBg, src.DiffContextBg}, {&t.DiffLineNumber, src.DiffLineNumber},
	} {
		if *p.dst == "" {
			*p.dst = p.src
		}
	}
}

// Apply overwrites every field of t with the named theme's values, in
// place — so every widget holding this *Theme pointer instantly renders
// the new palette. Returns false when name is unknown. The caller must
// trigger a re-render (and clear any colour-dependent cache).
func (t *Theme) Apply(name string) bool {
	src := Get(name)
	if src == nil {
		return false
	}
	*t = *src
	return true
}
