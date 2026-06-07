package ctxinject

import (
	"strings"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

func sampleData() Data {
	return Data{
		User: map[string]any{
			"id": "u-1", "name": "Paul", "region": "EU-West", "locale": "fr-FR",
			"roles": []string{"admin", "billing"},
		},
		App:     map[string]any{"id": "claude-code", "name": "Claude Code", "version": "1.0"},
		Agent:   map[string]any{"id": "main", "role": "coordinator"},
		Session: map[string]any{"goal": "ship the feature", "mode": "build", "turn": "3"},
		Now:     time.Date(2026, 6, 7, 14, 30, 0, 0, time.UTC),
	}
}

func TestRender_StaticText(t *testing.T) {
	out := Render([]schema.ContextSection{
		{Title: "Policy", Text: "Always answer in the user's language.", Priority: 1},
	}, sampleData())
	if !strings.Contains(out, "# Policy") || !strings.Contains(out, "Always answer") {
		t.Fatalf("static text not rendered: %q", out)
	}
}

func TestRender_TemplateInterpolation(t *testing.T) {
	out := Render([]schema.ContextSection{
		{Template: "User {{user.name}} in {{user.region}} — app {{app.name}}, {{date}}."},
	}, sampleData())
	want := "User Paul in EU-West — app Claude Code, 2026-06-07."
	if out != want {
		t.Fatalf("template:\n got %q\nwant %q", out, want)
	}
}

func TestRender_TemplateUnknownPathBlank(t *testing.T) {
	out := Render([]schema.ContextSection{
		{Template: "X-{{user.nope}}{{nothing.here}}-Y"},
	}, sampleData())
	if out != "X--Y" {
		t.Fatalf("unknown paths must blank: %q", out)
	}
}

func TestRender_BuiltinUser(t *testing.T) {
	out := Render([]schema.ContextSection{{Builtin: "user"}}, sampleData())
	for _, w := range []string{"Paul", "EU-West", "fr-FR", "admin, billing"} {
		if !strings.Contains(out, w) {
			t.Errorf("user builtin missing %q in %q", w, out)
		}
	}
}

func TestRender_BuiltinDatetimeIsSnapshot(t *testing.T) {
	out := Render([]schema.ContextSection{{Builtin: "datetime"}}, sampleData())
	if !strings.Contains(out, "2026-06-07") || !strings.Contains(out, "Sunday") || !strings.Contains(out, "snapshot") {
		t.Fatalf("datetime builtin: %q", out)
	}
}

func TestRender_UnknownBuiltinSkipped(t *testing.T) {
	out := Render([]schema.ContextSection{
		{Title: "Ghost", Builtin: "does_not_exist"},
		{Text: "kept"},
	}, sampleData())
	if strings.Contains(out, "Ghost") || !strings.Contains(out, "kept") {
		t.Fatalf("unknown builtin should be skipped: %q", out)
	}
}

func TestRender_WhenGate(t *testing.T) {
	d := sampleData()
	// present → rendered
	out := Render([]schema.ContextSection{
		{Template: "region is {{user.region}}", When: "user.region"},
	}, d)
	if out == "" {
		t.Fatal("when-present section should render")
	}
	// absent → dropped
	d.User["region"] = ""
	out = Render([]schema.ContextSection{
		{Template: "region is {{user.region}}", When: "user.region"},
		{Text: "fallback"},
	}, d)
	if strings.Contains(out, "region is") || !strings.Contains(out, "fallback") {
		t.Fatalf("when-absent section should drop: %q", out)
	}
}

func TestRender_PriorityOrder(t *testing.T) {
	out := Render([]schema.ContextSection{
		{Text: "second", Priority: 10},
		{Text: "first", Priority: 1},
		{Text: "third", Priority: 20},
	}, sampleData())
	if out != "first\n\nsecond\n\nthird" {
		t.Fatalf("priority order wrong: %q", out)
	}
}

func TestRender_EmptyWhenNoSections(t *testing.T) {
	if Render(nil, sampleData()) != "" {
		t.Fatal("no sections → empty")
	}
}

func TestMerge_AgentOverridesAppById(t *testing.T) {
	app := &schema.ContextBlock{Sections: []schema.ContextSection{
		{ID: "user", Builtin: "user", Priority: 1},
		{ID: "policy", Text: "app policy", Priority: 2},
	}}
	agent := &schema.ContextBlock{Sections: []schema.ContextSection{
		{ID: "policy", Text: "agent policy override", Priority: 2}, // same id → replace
		{ID: "extra", Text: "agent only", Priority: 3},             // new
	}}
	merged := Merge(app, agent)
	if len(merged) != 3 {
		t.Fatalf("want 3 merged sections, got %d: %+v", len(merged), merged)
	}
	out := Render(merged, sampleData())
	if strings.Contains(out, "app policy") || !strings.Contains(out, "agent policy override") {
		t.Errorf("agent must override app by id: %q", out)
	}
	if !strings.Contains(out, "agent only") || !strings.Contains(out, "Paul") {
		t.Errorf("merge must keep both app builtin and new agent section: %q", out)
	}
}

func TestRender_UserDataPrivacyAcrossUsers(t *testing.T) {
	// The same section rendered for two different users must show each user's own
	// data — proving it's pure/per-call (the leak guard the design depends on).
	sec := []schema.ContextSection{{Builtin: "user"}}
	a := Render(sec, Data{User: map[string]any{"name": "Alice", "region": "US"}})
	b := Render(sec, Data{User: map[string]any{"name": "Bob", "region": "JP"}})
	if !strings.Contains(a, "Alice") || strings.Contains(a, "Bob") {
		t.Errorf("user A leaked: %q", a)
	}
	if !strings.Contains(b, "Bob") || strings.Contains(b, "Alice") {
		t.Errorf("user B leaked: %q", b)
	}
}
