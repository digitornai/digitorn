package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/digitornai/digitorn-cli/internal/theme"
)

func kp(s string) tea.KeyMsg { return tea.KeyPressMsg{Text: s, Code: rune(s[0])} }

func ask(payload map[string]any) *AskForm {
	p := map[string]any{"id": "q1", "reason": "the question", "payload": payload}
	return NewAskForm(theme.Default(), p)
}

// A bare question is a free-text field : type, enter submits the text.
func TestAskForm_FreeText(t *testing.T) {
	f := ask(nil)
	if f == nil {
		t.Fatal("nil form")
	}
	f.ta.SetValue("hello world")
	act := f.Update(kp("enter"))
	if !act.Submit || act.Answer != "hello world" {
		t.Fatalf("free text submit = %+v, want answer %q", act, "hello world")
	}
}

// content → an editable textarea ; enter inserts a newline, ctrl+d submits the
// (possibly edited) content.
func TestAskForm_ContentEdit(t *testing.T) {
	f := ask(map[string]any{"content": "original text"})
	if f.fields[0].kind != fieldTextarea {
		t.Fatalf("content should be a textarea field, got %v", f.fields[0].kind)
	}
	if act := f.Update(kp("enter")); act.Submit {
		t.Fatal("enter must NOT submit a textarea (it inserts a newline)")
	}
	f.ta.SetValue("edited text")
	act := f.Update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
	if !act.Submit || act.Answer != "edited text" {
		t.Fatalf("ctrl+d submit = %+v, want %q", act, "edited text")
	}
}

// Single choice : arrow down then enter returns the highlighted option.
func TestAskForm_SingleChoice(t *testing.T) {
	f := ask(map[string]any{"choices": []any{"staging", "production", "canary"}})
	if f.fields[0].kind != fieldSelect {
		t.Fatalf("want select, got %v", f.fields[0].kind)
	}
	f.Update(kp("j")) // → production
	act := f.Update(kp("enter"))
	if !act.Submit || act.Answer != "production" {
		t.Fatalf("single choice = %+v, want %q", act, "production")
	}
}

// Multi-select : space toggles, enter confirms comma-joined picks.
func TestAskForm_MultiChoice(t *testing.T) {
	f := ask(map[string]any{"choices": []any{"a", "b", "c"}, "allow_multiple": true})
	if f.fields[0].kind != fieldMultiSelect {
		t.Fatalf("want multiselect, got %v", f.fields[0].kind)
	}
	f.Update(kp(" ")) // toggle a
	f.Update(kp("j")) // → b
	f.Update(kp("j")) // → c
	f.Update(kp(" ")) // toggle c
	act := f.Update(kp("enter"))
	if !act.Submit || act.Answer != "a, c" {
		t.Fatalf("multi choice = %+v, want %q", act, "a, c")
	}
}

// A structured form returns a JSON object of name→value, typed per field.
func TestAskForm_StructuredForm(t *testing.T) {
	f := ask(map[string]any{"form": []any{
		map[string]any{"name": "framework", "type": "select", "options": []any{"React", "Vue"}},
		map[string]any{"name": "ssr", "type": "boolean"},
		map[string]any{"name": "notes", "type": "text"},
	}})
	if !f.isForm || len(f.fields) != 3 {
		t.Fatalf("want a 3-field form, got isForm=%v n=%d", f.isForm, len(f.fields))
	}
	// field 0 (select) : pick Vue
	f.Update(kp("j"))
	// → field 1 (boolean) : pick Yes
	f.Update(kp("tab"))
	f.Update(kp("j"))
	// → field 2 (text)
	f.Update(kp("tab"))
	f.ta.SetValue("use app router")
	act := f.Update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
	if !act.Submit {
		t.Fatalf("ctrl+d should submit the form: %+v", act)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(act.Answer), &got); err != nil {
		t.Fatalf("form answer is not JSON: %q (%v)", act.Answer, err)
	}
	if got["framework"] != "Vue" {
		t.Errorf("framework = %v, want Vue", got["framework"])
	}
	if got["ssr"] != true {
		t.Errorf("ssr = %v, want true", got["ssr"])
	}
	if got["notes"] != "use app router" {
		t.Errorf("notes = %v, want 'use app router'", got["notes"])
	}
}

// Esc cancels from any modality.
func TestAskForm_Cancel(t *testing.T) {
	f := ask(map[string]any{"choices": []any{"x", "y"}})
	if act := f.Update(kp("esc")); !act.Cancelled {
		t.Fatalf("esc should cancel: %+v", act)
	}
}

// The card renders the question and choices without crashing, and shows the
// option text.
func TestAskForm_CardRenders(t *testing.T) {
	f := ask(map[string]any{"choices": []any{"alpha", "beta"}})
	out := f.Card(70)
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Fatalf("card missing choices:\n%s", out)
	}
}

// A very long question must NOT grow unbounded (it used to push the answer
// fields off-screen) : the heading is bounded to maxQuestionLines and SCROLLS
// (PgUp/PgDn) instead of being truncated, with a position indicator.
func TestAskForm_LongQuestionScrolls(t *testing.T) {
	long := strings.Repeat("this is a very long question that keeps going and going ", 30)
	f := ask(map[string]any{"choices": []any{"yes", "no"}})
	f.question = long

	q := f.renderQuestion(60)
	lines := strings.Split(q, "\n")
	if len(lines) > maxQuestionLines+1 { // +1 = the scroll indicator line
		t.Fatalf("question region not bounded : %d lines (max %d + indicator)", len(lines), maxQuestionLines)
	}
	if !f.qScrollable || !strings.Contains(q, "PgUp/PgDn") {
		t.Fatalf("a long question should be scrollable with an indicator:\n%s", q)
	}

	// PgDown advances the visible window (the full question stays reachable).
	before := f.qScroll
	f.Update(kp("pgdown"))
	f.renderQuestion(60) // re-clamps
	if f.qScroll <= before {
		t.Fatalf("pgdown should scroll the question (was %d, now %d)", before, f.qScroll)
	}

	// The choices must still render below the question in the full card.
	if card := f.Card(70); !strings.Contains(card, "yes") || !strings.Contains(card, "no") {
		t.Fatalf("answer choices missing under a long question:\n%s", card)
	}
}
