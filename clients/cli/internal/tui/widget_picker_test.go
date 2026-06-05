package tui

import (
	"testing"

	"github.com/mbathepaul/digitorn-cli/internal/theme"
)

func pickerItems() []PickerItem {
	return []PickerItem{
		{ID: "s1", Label: "deploy pipeline"},
		{ID: "s2", Label: "fix login bug"},
		{ID: "s3", Label: "refactor diff render"},
	}
}

func typeQuery(p *Picker, q string) {
	for _, r := range q {
		p.Update(kp(string(r)))
	}
}

func TestPicker_FuzzyFilterAndSelect(t *testing.T) {
	p := NewPicker(theme.Default(), "Sessions", pickerItems())
	if len(p.matches) != 3 {
		t.Fatalf("fresh picker should match all 3, got %d", len(p.matches))
	}
	typeQuery(p, "diff")
	if len(p.matches) != 1 {
		t.Fatalf("query 'diff' should match exactly 1 (refactor diff render), got %d", len(p.matches))
	}
	if it, ok := p.selectedItem(); !ok || it.ID != "s3" {
		t.Fatalf("filtered selection = %+v ok=%v, want s3", it, ok)
	}
	if act := p.Update(kp("enter")); act.Selected != "s3" {
		t.Fatalf("enter on filtered list = %+v, want Selected s3", act)
	}
}

func TestPicker_BackspaceAndClear(t *testing.T) {
	p := NewPicker(theme.Default(), "Sessions", pickerItems())
	typeQuery(p, "diff")
	p.Update(kp("backspace")) // "dif" — still matches s3
	if len(p.matches) != 1 {
		t.Fatalf("after backspace 'dif' should still match 1, got %d", len(p.matches))
	}
	p.Update(kp("ctrl+u")) // clear → all
	if len(p.matches) != 3 || p.query != "" {
		t.Fatalf("ctrl+u should clear the query and show all 3, got %d query=%q", len(p.matches), p.query)
	}
}

func TestPicker_NoMatchEnterCancels(t *testing.T) {
	p := NewPicker(theme.Default(), "Sessions", pickerItems())
	typeQuery(p, "zzzzq")
	if len(p.matches) != 0 {
		t.Fatalf("nonsense query should match nothing, got %d", len(p.matches))
	}
	if act := p.Update(kp("enter")); !act.Cancelled {
		t.Fatalf("enter with no matches should cancel, got %+v", act)
	}
}

func TestPicker_DeleteTwoPress(t *testing.T) {
	p := NewPicker(theme.Default(), "Sessions", pickerItems())
	p.deletable = true
	if act := p.Update(kp("ctrl+x")); act.Deleted != "" {
		t.Fatalf("first ctrl+x should only arm, got %+v", act)
	}
	if act := p.Update(kp("ctrl+x")); act.Deleted != "s1" {
		t.Fatalf("second ctrl+x should delete the armed row, got %+v", act)
	}
}

func TestPicker_LetterDoesNotNavigate(t *testing.T) {
	// 'j' used to move the cursor ; now it filters (search-box model).
	p := NewPicker(theme.Default(), "Sessions", pickerItems())
	p.Update(kp("j"))
	if p.query != "j" {
		t.Fatalf("'j' should type into the query, got %q", p.query)
	}
}
