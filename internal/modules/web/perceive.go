package web

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// This file builds the "page model": a compact, structured view of a page the
// agent can both READ (content, structure) and NAVIGATE (links = where to go,
// actions = what to click). It is derived purely from the (rendered) HTML DOM,
// so it works identically for the plain-HTTP path and the headless-render path.
//
// Refs ("e1", "e2", …) are stamped by the browser and only on elements that
// don't have one yet, so a node keeps its ref across re-renders and a replaced
// node's old ref resolves to nothing (a clear error) rather than to some other
// element. The plain-HTTP path has no browser, so it falls back to document
// order — fine, since no action can run without a live page.

// Collection bounds — keep the return token-efficient even on huge pages.
const (
	maxOutline    = 60
	maxLinks      = 100
	maxActions    = 60
	maxMedia      = 40
	maxLabelLen   = 140
	maxJSONLD     = 4
	linkHardCap   = 800 // collect this many before zone-priority trims to maxLinks
	actionHardCap = 400
)

type pageHeading struct {
	Level int    `json:"level"`
	Text  string `json:"text"`
}

type pageLink struct {
	Text string `json:"text"`
	URL  string `json:"url"`
	Zone string `json:"zone"` // nav | header | footer | content
	Ref  string `json:"ref"`

	hidden bool // no layout box; ranks last, never serialized
}

type pageAction struct {
	Type    string   `json:"type"` // button | form | input | select | tab | radio | checkbox
	Label   string   `json:"label"`
	Ref     string   `json:"ref"`
	Fields  []string `json:"fields,omitempty"`  // form field names
	Submit  string   `json:"submit,omitempty"`  // form submit label
	Group   string   `json:"group,omitempty"`   // radio/checkbox: shared name → same question
	Checked bool     `json:"checked,omitempty"` // radio/checkbox: currently selected

	hidden bool // no layout box; ranks last, never serialized
}

type pageMedia struct {
	Alt string `json:"alt,omitempty"`
	Src string `json:"src"`
	Ref string `json:"ref"`
}

type pageModel struct {
	Outline  []pageHeading  `json:"outline,omitempty"`
	Links    []pageLink     `json:"links,omitempty"`
	Actions  []pageAction   `json:"actions,omitempty"`
	Media    []pageMedia    `json:"media,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
	NavHints map[string]any `json:"nav_hints,omitempty"`
	Captcha  string         `json:"captcha,omitempty"` // recaptcha|hcaptcha|turnstile|… when a human-verification challenge is on the page
	Modal    string         `json:"modal,omitempty"`   // label of an open modal/dialog covering the page: act on ITS controls first
}

// empty reports whether the model carries no navigational signal (so callers can
// omit it from the response entirely).
func (m pageModel) empty() bool {
	return len(m.Outline) == 0 && len(m.Links) == 0 && len(m.Actions) == 0 &&
		len(m.Media) == 0 && len(m.Data) == 0 && len(m.NavHints) == 0
}

// buildPageModel walks the DOM once, assigns document-order refs, and collects
// the structured page model. baseURL resolves relative hrefs/srcs to absolute.
func buildPageModel(root *html.Node, baseURL string) pageModel {
	var base *url.URL
	if u, err := url.Parse(baseURL); err == nil && u.IsAbs() {
		base = u
	}

	var m pageModel
	m.Data = map[string]any{}
	seenLink := make(map[string]bool)
	var jsonld []any
	og := map[string]string{}
	counter := 0

	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				walk(c)
				continue
			}
			counter++
			// Prefer the browser-stamped ref (data-dgn-ref) so a ref resolves to
			// the exact live element an action targets; fall back to document
			// order for the plain-HTTP path (no browser, no actions possible).
			ref := attr(c, refAttr)
			if ref == "" {
				ref = "e" + itoa(counter)
			}

			switch c.DataAtom {
			case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
				if len(m.Outline) < maxOutline {
					if txt := label(c); txt != "" {
						m.Outline = append(m.Outline, pageHeading{Level: headingLevel(c.DataAtom), Text: txt})
					}
				}

			case atom.A:
				href := strings.TrimSpace(attr(c, "href"))
				if isNavigableHref(href) && len(m.Links) < linkHardCap {
					abs := resolveURL(base, href)
					if abs != "" && !seenLink[abs] {
						seenLink[abs] = true
						m.Links = append(m.Links, pageLink{
							Text: label(c), URL: abs, Zone: zoneOf(c), Ref: ref,
							hidden: attr(c, hiddenAttr) != "",
						})
					}
				}

			case atom.Button:
				if len(m.Actions) < actionHardCap {
					m.Actions = append(m.Actions, pageAction{
						Type: roleOr(c, "button"), Label: buttonLabel(c), Ref: ref, hidden: attr(c, hiddenAttr) != "",
					})
				}

			case atom.Form:
				if len(m.Actions) < actionHardCap {
					m.Actions = append(m.Actions, pageAction{
						Type: "form", Label: firstNonEmpty(attr(c, "aria-label"), attr(c, "name"), "form"),
						Ref: ref, Fields: formFields(c), Submit: formSubmitLabel(c), hidden: attr(c, hiddenAttr) != "",
					})
				}

			case atom.Input:
				t := strings.ToLower(attr(c, "type"))
				if t == "hidden" {
					break
				}
				if t == "submit" || t == "button" {
					if len(m.Actions) < actionHardCap {
						m.Actions = append(m.Actions, pageAction{
							Type: "button", Label: firstNonEmpty(attr(c, "value"), attr(c, "aria-label"), "submit"), Ref: ref, hidden: attr(c, hiddenAttr) != "",
						})
					}
				} else if t == "radio" || t == "checkbox" {
					if len(m.Actions) < actionHardCap {
						m.Actions = append(m.Actions, pageAction{
							Type: t, Label: choiceLabel(c), Group: attr(c, "name"),
							Checked: attr(c, checkedAttr) != "" || hasAttr(c, "checked"), Ref: ref, hidden: attr(c, hiddenAttr) != "",
						})
					}
				} else if len(m.Actions) < actionHardCap {
					m.Actions = append(m.Actions, pageAction{
						Type: "input", Label: inputLabel(c), Ref: ref, hidden: attr(c, hiddenAttr) != "",
					})
				}

			case atom.Textarea, atom.Select:
				if len(m.Actions) < actionHardCap {
					m.Actions = append(m.Actions, pageAction{
						Type: tagType(c.DataAtom), Label: inputLabel(c), Ref: ref, hidden: attr(c, hiddenAttr) != "",
					})
				}

			case atom.Img:
				src := strings.TrimSpace(attr(c, "src"))
				if src != "" && len(m.Media) < maxMedia {
					m.Media = append(m.Media, pageMedia{
						Alt: truncate(strings.TrimSpace(attr(c, "alt")), maxLabelLen),
						Src: resolveURL(base, src), Ref: ref,
					})
				}

			case atom.Meta:
				if p := attr(c, "property"); strings.HasPrefix(strings.ToLower(p), "og:") {
					if v := strings.TrimSpace(attr(c, "content")); v != "" {
						og[strings.ToLower(p)] = v
					}
				}

			case atom.Script:
				if strings.EqualFold(attr(c, "type"), "application/ld+json") && len(jsonld) < maxJSONLD {
					if v := parseJSONLD(textOf(c)); v != nil {
						jsonld = append(jsonld, v)
					}
				}

			case atom.Link:
				if strings.EqualFold(attr(c, "rel"), "next") {
					if abs := resolveURL(base, attr(c, "href")); abs != "" {
						setNavHint(&m, "next_page", abs)
					}
				}
			}

			// Generic role-based tabs/buttons on non-native elements.
			if r := strings.ToLower(attr(c, "role")); r == "tab" || r == "button" {
				if c.DataAtom != atom.Button && c.DataAtom != atom.A && len(m.Actions) < actionHardCap {
					m.Actions = append(m.Actions, pageAction{Type: r, Label: label(c), Ref: ref, hidden: attr(c, hiddenAttr) != ""})
				}
			}

			if m.Captcha == "" {
				if ct := captchaType(c); ct != "" {
					m.Captcha = ct
				}
			}
			if m.Modal == "" && attr(c, hiddenAttr) == "" {
				if isModal(c) {
					m.Modal = firstNonEmpty(modalLabel(c), "dialog")
				}
			}

			walk(c)
		}
	}
	walk(root)

	m.Links = prioritizeLinks(m.Links, maxLinks)
	m.Actions = prioritizeActions(m.Actions, maxActions)

	if len(jsonld) > 0 {
		m.Data["jsonld"] = jsonld
	}
	if len(og) > 0 {
		m.Data["opengraph"] = og
	}
	if len(m.Data) == 0 {
		m.Data = nil
	}
	detectPaginationHints(&m)
	return m
}

// ── helpers ──────────────────────────────────────────────────────────────

// prioritizeLinks trims to max: what's on screen first, then content over
// nav/footer/header. Without the visibility rank a single collapsed mega-menu
// (GitHub's ~180 hidden language filters) crowds out every real link.
func prioritizeLinks(links []pageLink, max int) []pageLink {
	if len(links) <= max {
		return links
	}
	zoneRank := func(zone string) int {
		switch zone {
		case "content":
			return 0
		case "nav":
			return 1
		case "footer":
			return 2
		default: // header + anything unclassified
			return 3
		}
	}
	rank := func(l pageLink) int {
		r := zoneRank(l.Zone)
		if l.hidden {
			r += 10
		}
		return r
	}
	sort.SliceStable(links, func(i, j int) bool {
		return rank(links[i]) < rank(links[j])
	})
	return links[:max]
}

func headingLevel(a atom.Atom) int {
	switch a {
	case atom.H1:
		return 1
	case atom.H2:
		return 2
	case atom.H3:
		return 3
	case atom.H4:
		return 4
	case atom.H5:
		return 5
	default:
		return 6
	}
}

func tagType(a atom.Atom) string {
	if a == atom.Textarea {
		return "input"
	}
	return "select"
}

// label returns the trimmed, collapsed, truncated visible text of a node.
func label(n *html.Node) string {
	return truncate(strings.TrimSpace(collapseWS(textOf(n))), maxLabelLen)
}

// captchaType names a human-verification widget on the page (empty = none), so
// the agent knows it must hand off to the user rather than try to solve it.
func captchaType(n *html.Node) string {
	src := strings.ToLower(attr(n, "src"))
	switch {
	case strings.Contains(src, "recaptcha"):
		return "recaptcha"
	case strings.Contains(src, "hcaptcha"):
		return "hcaptcha"
	case strings.Contains(src, "challenges.cloudflare.com"), strings.Contains(src, "turnstile"):
		return "turnstile"
	case strings.Contains(src, "arkoselabs"), strings.Contains(src, "funcaptcha"):
		return "arkose"
	}
	blob := strings.ToLower(attr(n, "class") + " " + attr(n, "id"))
	switch {
	case strings.Contains(blob, "g-recaptcha"):
		return "recaptcha"
	case strings.Contains(blob, "h-captcha"):
		return "hcaptcha"
	case strings.Contains(blob, "cf-turnstile"):
		return "turnstile"
	}
	return ""
}

// isModal reports whether a node is an open modal/dialog covering the page.
// Beyond the accessible markers, it accepts a visible CONTAINER whose class/id
// carries a modal token — many real modals (Bootstrap-style .modal) declare no
// role. Restricting to containers keeps a "open-modal" button from matching.
func isModal(n *html.Node) bool {
	if strings.EqualFold(attr(n, "aria-modal"), "true") {
		return true
	}
	if strings.EqualFold(attr(n, "role"), "dialog") || strings.EqualFold(attr(n, "role"), "alertdialog") {
		return true
	}
	if n.DataAtom == atom.Dialog && hasAttr(n, "open") {
		return true
	}
	switch n.DataAtom {
	case atom.Div, atom.Section, atom.Aside:
		for _, tok := range tokens(attr(n, "class") + " " + attr(n, "id")) {
			switch tok {
			case "modal", "dialog", "lightbox", "popup":
				return true
			}
		}
	}
	return false
}

// tokens splits a class/id blob on spaces and hyphens into lowercase words, so
// "modal-content" yields "modal" and "content".
func tokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == ' ' || r == '-' || r == '_' || r == '\t' || r == '\n'
	})
}

// modalLabel is a modal's accessible name: aria-label, else its first heading.
func modalLabel(n *html.Node) string {
	if s := attr(n, "aria-label"); s != "" {
		return truncate(s, maxLabelLen)
	}
	if h := findFirst(n, func(e *html.Node) bool {
		switch e.DataAtom {
		case atom.H1, atom.H2, atom.H3, atom.H4:
			return true
		}
		return false
	}); h != nil {
		return label(h)
	}
	return ""
}

func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

// choiceLabel is the visible text of a radio/checkbox option: the enclosing
// label's text (minus the control), else value/aria-label, so the agent sees
// "Masculin" instead of the raw field name.
func choiceLabel(n *html.Node) string {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.DataAtom == atom.Label {
			if t := label(p); t != "" {
				return t
			}
		}
	}
	if s := attr(n, "aria-label"); s != "" {
		return truncate(s, maxLabelLen)
	}
	if s := attr(n, "value"); s != "" {
		return truncate(s, maxLabelLen)
	}
	// Text sitting right after the input (common: <input><span>Masculin</span>).
	for s := n.NextSibling; s != nil; s = s.NextSibling {
		if txt := strings.TrimSpace(collapseWS(textOf(s))); txt != "" {
			return truncate(txt, maxLabelLen)
		}
	}
	return truncate(attr(n, "name"), maxLabelLen)
}

// buttonLabel prefers visible text, then aria-label / value / title.
func buttonLabel(n *html.Node) string {
	if t := label(n); t != "" {
		return t
	}
	return truncate(firstNonEmpty(attr(n, "aria-label"), attr(n, "value"), attr(n, "title"), "button"), maxLabelLen)
}

// inputLabel builds a human label for an input-like control.
func inputLabel(n *html.Node) string {
	return truncate(firstNonEmpty(
		attr(n, "aria-label"), attr(n, "placeholder"), attr(n, "name"),
		attr(n, "id"), strings.ToLower(attr(n, "type")), "input",
	), maxLabelLen)
}

// roleOr returns the element's ARIA role if present, else the fallback.
func roleOr(n *html.Node, fallback string) string {
	if r := strings.ToLower(strings.TrimSpace(attr(n, "role"))); r != "" {
		return r
	}
	return fallback
}

// zoneOf classifies a node by its nearest structural ancestor.
func zoneOf(n *html.Node) string {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type != html.ElementNode {
			continue
		}
		switch p.DataAtom {
		case atom.Nav:
			return "nav"
		case atom.Footer:
			return "footer"
		case atom.Header:
			return "header"
		case atom.Main, atom.Article:
			return "content"
		}
	}
	return "content"
}

// formFields lists the names of a form's fillable controls (bounded).
func formFields(form *html.Node) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode {
				switch c.DataAtom {
				case atom.Input, atom.Textarea, atom.Select:
					t := strings.ToLower(attr(c, "type"))
					if t != "hidden" && t != "submit" && t != "button" {
						name := firstNonEmpty(attr(c, "name"), attr(c, "id"), attr(c, "placeholder"))
						if name != "" && !seen[name] && len(out) < 20 {
							seen[name] = true
							out = append(out, name)
						}
					}
				}
			}
			walk(c)
		}
	}
	walk(form)
	return out
}

// formSubmitLabel returns the label of a form's submit control, if any.
func formSubmitLabel(form *html.Node) string {
	btn := findFirst(form, func(n *html.Node) bool {
		if n.DataAtom == atom.Button {
			return !strings.EqualFold(attr(n, "type"), "button")
		}
		return n.DataAtom == atom.Input && strings.EqualFold(attr(n, "type"), "submit")
	})
	if btn == nil {
		return ""
	}
	if btn.DataAtom == atom.Input {
		return truncate(firstNonEmpty(attr(btn, "value"), "submit"), maxLabelLen)
	}
	return buttonLabel(btn)
}

// isNavigableHref rejects empty, fragment-only, and non-navigational schemes.
func isNavigableHref(href string) bool {
	if href == "" || href == "#" || strings.HasPrefix(href, "#") {
		return false
	}
	lower := strings.ToLower(href)
	for _, bad := range []string{"javascript:", "mailto:", "tel:", "data:", "blob:"} {
		if strings.HasPrefix(lower, bad) {
			return false
		}
	}
	return true
}

// resolveURL turns a possibly-relative href into an absolute URL against base.
func resolveURL(base *url.URL, href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	ref, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if base == nil {
		if ref.IsAbs() {
			return ref.String()
		}
		return href
	}
	return base.ResolveReference(ref).String()
}

// parseJSONLD parses a <script type=ld+json> body, returning nil on error.
func parseJSONLD(s string) any {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 100<<10 {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil
	}
	return v
}

// detectPaginationHints scans collected links/actions for "next" / "load more"
// affordances and records them under nav_hints.
func detectPaginationHints(m *pageModel) {
	for _, l := range m.Links {
		lt := strings.ToLower(l.Text)
		if lt == "next" || lt == "suivant" || lt == "next page" || lt == "page suivante" || lt == "›" || lt == "»" {
			setNavHint(m, "next_page", l.URL)
			break
		}
	}
	for _, a := range m.Actions {
		lt := strings.ToLower(a.Label)
		if strings.Contains(lt, "load more") || strings.Contains(lt, "charger plus") ||
			strings.Contains(lt, "show more") || strings.Contains(lt, "voir plus") {
			setNavHint(m, "has_more", true)
			setNavHint(m, "load_more_ref", a.Ref)
			break
		}
	}
}

func setNavHint(m *pageModel, key string, val any) {
	if m.NavHints == nil {
		m.NavHints = map[string]any{}
	}
	if _, exists := m.NavHints[key]; !exists {
		m.NavHints[key] = val
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && (s[cut]&0xC0) == 0x80 { // don't split a UTF-8 rune
		cut--
	}
	return s[:cut] + "…"
}

// itoa is a tiny int→string without importing strconv at call sites.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// prioritizeActions trims to max, keeping on-screen controls over ones with no
// layout box (collapsed menus, hidden dialogs).
func prioritizeActions(actions []pageAction, max int) []pageAction {
	if len(actions) <= max {
		return actions
	}
	sort.SliceStable(actions, func(i, j int) bool {
		return !actions[i].hidden && actions[j].hidden
	})
	return actions[:max]
}
