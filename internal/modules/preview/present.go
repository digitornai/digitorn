package preview

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// What comes back from a page is far larger than what the agent needs to decide
// its next move. A real dashboard reports 99 elements and 2 000 characters of
// text — roughly 2 500 tokens — and the agent inspects after every action, so
// walking through an app burns its context in a handful of turns.
//
// Two reductions, neither of which costs it anything:
//
//   - An UNCHANGED page is already described in the conversation. Re-sending the
//     same list of elements adds no information the agent does not have; a note
//     saying so is enough, and its earlier refs still resolve.
//   - On a changed page, what matters is what can be ACTED ON and what is
//     BROKEN. Failures stay whole — they are small and they are the point.
//     Elements and prose are ranked and trimmed, with the full payload one
//     parameter away.
const (
	compactElements = 50
	compactText     = 1500
)

// rank orders elements by how likely the agent is to need them: things it can
// type into or press first, structure last.
func rank(role string) int {
	switch role {
	case "field":
		return 0
	case "button":
		return 1
	case "link":
		return 2
	default:
		return 3
	}
}

// fingerprint identifies what the page shows, not when it was measured: same
// route, same actionable surface, same words. Failures are deliberately left
// out — a new error must always reach the agent.
func fingerprint(s Snapshot) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%t|%t|%d|", s.URL, s.Ready, s.Blank, len(s.Elements))
	for _, e := range s.Elements {
		fmt.Fprintf(h, "%s\x1f%s\x1f%s\x1e", e.Role, e.Text, e.Value)
	}
	fmt.Fprintf(h, "|%d|%s", len(s.Text), s.Viewport)
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// present trims a snapshot for the agent. unchanged says the page is identical
// to the one it was last shown; full asks for everything regardless.
func present(s Snapshot, unchanged, full, wantText bool) (Snapshot, string) {
	if full {
		if !wantText {
			s.Text = ""
		}
		return s, ""
	}

	if unchanged {
		n := len(s.Elements)
		s.Elements = nil
		s.Text = ""
		s.Storage = nil
		return s, fmt.Sprintf(
			"Screen unchanged since your last inspect (same route, same %d elements) — "+
				"the description above still applies and its refs still resolve. "+
				"Anything new is in the failures below. Pass full:true to see it all again.", n)
	}

	var note string
	if len(s.Elements) > compactElements {
		ordered := append([]Element(nil), s.Elements...)
		sort.SliceStable(ordered, func(i, j int) bool { return rank(ordered[i].Role) < rank(ordered[j].Role) })
		dropped := len(ordered) - compactElements
		s.Elements = ordered[:compactElements]
		note = fmt.Sprintf("Showing the %d elements you are most likely to act on; %d more (mostly headings and repeated rows) are hidden — pass full:true for the lot.",
			compactElements, dropped)
	}

	if !wantText {
		s.Text = ""
	} else if len(s.Text) > compactText {
		cut := strings.LastIndex(s.Text[:compactText], "\n")
		if cut < compactText/2 {
			cut = compactText
		}
		s.Text = s.Text[:cut] + "\n…"
		if note != "" {
			note += " "
		}
		note += "Text truncated — pass full:true to read the whole page."
	}
	return s, note
}
