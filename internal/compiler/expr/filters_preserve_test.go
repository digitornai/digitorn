package expr

import "testing"

func TestPassthroughPreservesFilters(t *testing.T) {
	e := NewEngine()
	got, err := e.ResolveString(`{"name":{{event.payload.subject | json}}}`)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := `{"name":{{event.payload.subject | json}}}`
	if got != want {
		t.Errorf("passthrough = %q, want %q", got, want)
	}
}
