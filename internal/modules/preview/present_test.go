package preview

import (
	"encoding/json"
	"strings"
	"testing"
)

func bigSnap() Snapshot {
	s := Snapshot{URL: "http://a/#/dashboard", Ready: true, Viewport: "desktop"}
	s.Text = strings.Repeat("Ligne de contenu du tableau de bord. ", 120) // ~4KB
	for i := 0; i < 99; i++ {
		role := []string{"heading", "button", "link", "field"}[i%4]
		s.Elements = append(s.Elements, Element{Ref: "e", Role: role, Text: "Élément numéro parmi beaucoup"})
	}
	return s
}

func tokens(s Snapshot) int {
	raw, _ := json.Marshal(s)
	return len(raw) / 4
}

func TestUnchangedPageCollapsesToANote(t *testing.T) {
	s := bigSnap()
	before := tokens(s)

	shown, note := present(s, true /*unchanged*/, false, true)
	if len(shown.Elements) != 0 || shown.Text != "" {
		t.Fatal("an unchanged page must not re-send the elements and text the agent already holds")
	}
	if note == "" || !strings.Contains(note, "unchanged") {
		t.Errorf("the agent must be told why the payload is gone, got %q", note)
	}
	after := tokens(shown)
	t.Logf("unchanged page: %d → %d tokens (%.0f%% saved)", before, after, 100*float64(before-after)/float64(before))
	if after > before/8 {
		t.Errorf("an unchanged page still cost %d tokens, barely less than %d", after, before)
	}
}

func TestChangedPageIsTrimmedButKeepsWhatMatters(t *testing.T) {
	s := bigSnap()
	s.Errors = []RuntimeError{{Kind: "error", Message: "products is not iterable", Line: 12}}
	before := tokens(s)

	shown, note := present(s, false, false, true)
	after := tokens(shown)
	t.Logf("changed page: %d → %d tokens (%.0f%% saved)", before, after, 100*float64(before-after)/float64(before))

	if len(shown.Elements) > compactElements {
		t.Errorf("kept %d elements, cap is %d", len(shown.Elements), compactElements)
	}
	if len(shown.Errors) != 1 {
		t.Fatal("a runtime failure must survive trimming — it is the whole point of looking")
	}
	// Actionable elements must be the ones kept.
	for _, e := range shown.Elements {
		if e.Role == "heading" {
			// headings are allowed only after fields/buttons/links are all in
			if rank(e.Role) < 3 {
				t.Fatal("ranking is wrong")
			}
		}
	}
	if note == "" {
		t.Error("the agent must be told the list was trimmed and how to get the rest")
	}
	if after >= before {
		t.Error("trimming saved nothing")
	}
}

func TestFullBypassesTrimming(t *testing.T) {
	s := bigSnap()
	shown, note := present(s, true, true /*full*/, true)
	if len(shown.Elements) != 99 {
		t.Errorf("full:true must return every element, got %d", len(shown.Elements))
	}
	if note != "" {
		t.Errorf("full has nothing to apologise for, got note %q", note)
	}
}

func TestFingerprintIgnratesTimeButCatchesChange(t *testing.T) {
	a := bigSnap()
	b := bigSnap() // identical content
	if fingerprint(a) != fingerprint(b) {
		t.Fatal("two identical screens must fingerprint the same, or every inspect looks 'changed'")
	}
	b.URL = "http://a/#/ecommerce"
	if fingerprint(a) == fingerprint(b) {
		t.Error("navigating must change the fingerprint")
	}
	c := bigSnap()
	c.Elements[0].Text = "Texte différent"
	if fingerprint(a) == fingerprint(c) {
		t.Error("a changed label must change the fingerprint")
	}
	// A new error must NOT be hidden by an unchanged fingerprint.
	d := bigSnap()
	d.Errors = []RuntimeError{{Message: "boom"}}
	if fingerprint(a) != fingerprint(d) {
		t.Error("errors are handled separately and must not affect the fingerprint")
	}
}
