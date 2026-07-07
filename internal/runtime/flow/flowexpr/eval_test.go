package flowexpr

import "testing"

type mapCtx map[string]any

func (m mapCtx) Lookup(path []string) (any, bool) {
	var cur any = map[string]any(m)
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func TestEval(t *testing.T) {
	ctx := mapCtx{
		"category":  "refund",
		"ticket":    map[string]any{"priority": "p0", "score": float64(7)},
		"approvals": map[string]any{"gate": "approve"},
		"count":     float64(3),
		"flagged":   true,
	}
	cases := []struct {
		expr string
		want bool
	}{
		{"category == 'refund'", true},
		{"category == 'tech'", false},
		{"category != 'tech'", true},
		{`category == "refund"`, true},
		{"approvals.gate == 'approve'", true},
		{"ticket.priority == 'p0'", true},
		{"ticket.priority == 'p1'", false},
		{"default", true},
		{"true", true},
		{"false", false},
		{"flagged", true},
		{"not flagged", false},
		{"count > 2", true},
		{"count >= 3", true},
		{"count < 3", false},
		{"count <= 3", true},
		{"ticket.score == 7", true},
		{"ticket.score > 10", false},
		{"category == 'refund' and ticket.priority == 'p0'", true},
		{"category == 'refund' and ticket.priority == 'p1'", false},
		{"category == 'tech' or approvals.gate == 'approve'", true},
		{"category == 'tech' or approvals.gate == 'deny'", false},
		{"not (category == 'tech')", true},
		{"(category == 'refund' or category == 'tech') and flagged", true},
		{"category == 'refund' && flagged", true},
		{"category == 'tech' || flagged", true},
		{"missing == 'x'", false},
		{"missing", false},
		{"not missing", true},
		{"unknown.deep.path == 'x'", false},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			got, err := EvalString(tc.expr, ctx)
			if err != nil {
				t.Fatalf("eval %q: %v", tc.expr, err)
			}
			if got != tc.want {
				t.Fatalf("eval %q = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	bad := []string{
		"category ==",
		"== 'x'",
		"(category == 'x'",
		"category = 'x'",
		"category == 'x' extra",
		"'unterminated",
	}
	for _, src := range bad {
		t.Run(src, func(t *testing.T) {
			if _, err := Compile(src); err == nil {
				t.Fatalf("expected parse error for %q", src)
			}
		})
	}
}

func TestCompileCache(t *testing.T) {
	a, err := Compile("category == 'refund'")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Compile("category == 'refund'")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("expected memoised program identity")
	}
}
