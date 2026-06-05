package web

import (
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// stripTags are elements whose entire subtree is dropped before rendering —
// scripts, styling, chrome and embedded frames carry no readable content and
// only add noise (or, for script, injection bait).
var stripTags = map[atom.Atom]bool{
	atom.Script:   true,
	atom.Style:    true,
	atom.Nav:      true,
	atom.Footer:   true,
	atom.Header:   true,
	atom.Noscript: true,
	atom.Svg:      true,
	atom.Iframe:   true,
	atom.Form:     true,
	atom.Template: true,
}

// blockTags force a line break around their content so the flattened text
// keeps paragraph/structure boundaries instead of running together.
var blockTags = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Section: true, atom.Article: true,
	atom.Ul: true, atom.Ol: true, atom.Table: true, atom.Tr: true,
	atom.Blockquote: true, atom.Pre: true, atom.Main: true, atom.Aside: true,
	atom.Dl: true, atom.Dd: true, atom.Dt: true, atom.Figure: true,
}

// parseHTML parses a document, returning nil when the input is not parseable.
func parseHTML(doc string) *html.Node {
	root, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		return nil
	}
	return root
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// findFirst returns the first element matching pred in document order.
func findFirst(n *html.Node, pred func(*html.Node) bool) *html.Node {
	if n == nil {
		return nil
	}
	if n.Type == html.ElementNode && pred(n) {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if got := findFirst(c, pred); got != nil {
			return got
		}
	}
	return nil
}

// extractTitle returns the trimmed <title> text.
func extractTitle(root *html.Node) string {
	t := findFirst(root, func(n *html.Node) bool { return n.DataAtom == atom.Title })
	if t == nil {
		return ""
	}
	return strings.TrimSpace(textOf(t))
}

// extractMetaDescription returns the content of <meta name="description">.
func extractMetaDescription(root *html.Node) string {
	m := findFirst(root, func(n *html.Node) bool {
		return n.DataAtom == atom.Meta && strings.EqualFold(attr(n, "name"), "description")
	})
	if m == nil {
		return ""
	}
	return strings.TrimSpace(attr(m, "content"))
}

// textOf returns the concatenated raw text of a node's subtree.
func textOf(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return sb.String()
}

// render flattens a node subtree to text (markdown=false) or Markdown
// (markdown=true): noise subtrees are dropped, block elements get line breaks,
// links/headings/lists/code render in Markdown when asked.
func render(root *html.Node, markdown bool) string {
	var sb strings.Builder
	var walk func(n *html.Node, inPre bool)
	walk = func(n *html.Node, inPre bool) {
		switch n.Type {
		case html.TextNode:
			if inPre {
				sb.WriteString(n.Data)
			} else {
				sb.WriteString(collapseWS(n.Data))
			}
			return
		case html.ElementNode:
		default:
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c, inPre)
			}
			return
		}

		if stripTags[n.DataAtom] {
			return
		}

		switch n.DataAtom {
		case atom.Br:
			sb.WriteByte('\n')
			return
		case atom.Hr:
			sb.WriteString("\n\n---\n\n")
			return
		case atom.A:
			if markdown {
				href := attr(n, "href")
				label := strings.TrimSpace(collapseWS(textOf(n)))
				if href != "" && label != "" {
					sb.WriteString("[" + label + "](" + href + ")")
					return
				}
			}
		case atom.Img:
			if markdown {
				if src := attr(n, "src"); src != "" {
					sb.WriteString("![" + attr(n, "alt") + "](" + src + ")")
				}
			}
			return
		case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
			sb.WriteString("\n\n")
			if markdown {
				sb.WriteString(strings.Repeat("#", int(n.DataAtom-atom.H1)+1) + " ")
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c, inPre)
			}
			sb.WriteString("\n\n")
			return
		case atom.Li:
			sb.WriteByte('\n')
			if markdown {
				sb.WriteString("- ")
			} else {
				sb.WriteString("• ")
			}
		case atom.Code:
			if markdown {
				sb.WriteByte('`')
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c, inPre)
				}
				sb.WriteByte('`')
				return
			}
		case atom.Pre:
			if markdown {
				sb.WriteString("\n\n```\n")
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c, true)
				}
				sb.WriteString("\n```\n\n")
				return
			}
			inPre = true
		}

		block := blockTags[n.DataAtom]
		if block {
			sb.WriteByte('\n')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inPre)
		}
		if block {
			sb.WriteByte('\n')
		}
	}
	walk(root, false)
	return tidy(sb.String())
}

// collapseWS applies the HTML whitespace-collapsing rule for non-<pre> text:
// every run of whitespace becomes a single space. A leading/trailing space is
// preserved (as one space) so the boundary between adjacent inline text nodes
// — e.g. "with a " followed by a link — is not lost.
func collapseWS(s string) string {
	if strings.TrimSpace(s) == "" {
		if s == "" {
			return ""
		}
		return " "
	}
	core := strings.Join(strings.Fields(s), " ")
	if len(strings.TrimLeftFunc(s, unicode.IsSpace)) < len(s) {
		core = " " + core
	}
	if len(strings.TrimRightFunc(s, unicode.IsSpace)) < len(s) {
		core += " "
	}
	return core
}

// tidy collapses 3+ blank lines to a paragraph break, trims trailing spaces on
// each line, and trims the whole string.
func tidy(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight(ln, " \t")
	}
	s = strings.Join(lines, "\n")
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(s)
}

// mainContent returns the most content-bearing subtree: an explicit <main> or
// <article> when present, else <body>, else the document. Chrome (nav/footer/
// header/script/style/form/iframe) is dropped by render via stripTags.
func mainContent(root *html.Node) *html.Node {
	if m := findFirst(root, func(n *html.Node) bool {
		return n.DataAtom == atom.Main || n.DataAtom == atom.Article
	}); m != nil {
		return m
	}
	if b := findFirst(root, func(n *html.Node) bool { return n.DataAtom == atom.Body }); b != nil {
		return b
	}
	return root
}

// selectNodes returns elements matching a comma-separated list of simple
// selectors: a tag name ("article"), a class (".content") or an id
// ("#main"). It is intentionally minimal — enough to target the common
// content containers without a full CSS engine.
func selectNodes(root *html.Node, selector string) []*html.Node {
	var out []*html.Node
	for _, raw := range strings.Split(selector, ",") {
		sel := strings.TrimSpace(raw)
		if sel == "" {
			continue
		}
		match := selectorMatcher(sel)
		if match == nil {
			continue
		}
		var walk func(*html.Node)
		walk = func(n *html.Node) {
			if n.Type == html.ElementNode && match(n) {
				out = append(out, n)
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
		walk(root)
		if len(out) > 0 {
			return out
		}
	}
	return out
}

func selectorMatcher(sel string) func(*html.Node) bool {
	switch {
	case strings.HasPrefix(sel, "."):
		want := sel[1:]
		return func(n *html.Node) bool { return hasClass(n, want) }
	case strings.HasPrefix(sel, "#"):
		want := sel[1:]
		return func(n *html.Node) bool { return attr(n, "id") == want }
	default:
		want := strings.ToLower(sel)
		return func(n *html.Node) bool { return n.Data == want }
	}
}

func hasClass(n *html.Node, want string) bool {
	for _, c := range strings.Fields(attr(n, "class")) {
		if c == want {
			return true
		}
	}
	return false
}

// injectionMarkers are phrases that, embedded in fetched content, suggest an
// attempt to hijack the model's instructions. Matching is a best-effort
// advisory warning — never a hard block.
var injectionMarkers = []string{
	"ignore previous instructions",
	"ignore all instructions",
	"disregard your instructions",
	"you are now",
	"new instructions:",
	"system prompt:",
	"forget everything",
	"<|im_start|>",
	"[inst]",
	"<<sys>>",
}

// scanForInjection returns a warning when content contains a known prompt-
// injection marker, or "" when clean.
func scanForInjection(content string) string {
	lower := strings.ToLower(content)
	for _, m := range injectionMarkers {
		if strings.Contains(lower, m) {
			return "potential prompt injection: content contains " + strconvQuote(m)
		}
	}
	return ""
}

func strconvQuote(s string) string { return "\"" + s + "\"" }
