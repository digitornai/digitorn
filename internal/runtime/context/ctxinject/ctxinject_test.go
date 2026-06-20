package ctxinject

import (
	"os"
	"path/filepath"
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

func TestRender_WhenComparison(t *testing.T) {
	cases := []struct {
		when string
		data Data
		want bool
	}{
		{"session.context_pct > 60", Data{Session: map[string]any{"context_pct": "75"}}, true},
		{"session.context_pct > 60", Data{Session: map[string]any{"context_pct": "45"}}, false},
		{"session.context_pct >= 60", Data{Session: map[string]any{"context_pct": "60"}}, true},
		{"session.context_pct < 80", Data{Session: map[string]any{"context_pct": "75"}}, true},
		{"session.context_pct <= 60", Data{Session: map[string]any{"context_pct": "61"}}, false},
		{"session.context_pct == 50", Data{Session: map[string]any{"context_pct": "50"}}, true},
		{"session.context_pct != 50", Data{Session: map[string]any{"context_pct": "75"}}, true},
		{"session.turn > 10", Data{Session: map[string]any{"turn": "5"}}, false},
		{"session.missing > 0", Data{Session: map[string]any{}}, false},
	}
	for _, c := range cases {
		got := Render([]schema.ContextSection{
			{Text: "shown", When: c.when},
		}, c.data) != ""
		if got != c.want {
			t.Errorf("when=%q data=%v: got rendered=%v want %v", c.when, c.data.Session, got, c.want)
		}
	}
}

func TestRender_FileSingle(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# Project rules\nAlways write tests."), 0600); err != nil {
		t.Fatal(err)
	}
	d := sampleData()
	d.Session["workdir"] = dir
	out := Render([]schema.ContextSection{
		{ID: "mem", File: "AGENTS.md", Priority: 1},
	}, d)
	if !strings.Contains(out, "Always write tests") {
		t.Fatalf("file content not injected: %q", out)
	}
	if strings.Contains(out, "## AGENTS.md") {
		t.Fatal("single file must not add header")
	}
	if !strings.Contains(out, "<system-reminder>") {
		t.Fatal("file section must be wrapped in system-reminder")
	}
}

func TestRender_FileMultiple(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("agents rule"), 0600)
	os.WriteFile(filepath.Join(dir, "DIGITORN.md"), []byte("digitorn rule"), 0600)
	d := sampleData()
	d.Session["workdir"] = dir
	out := Render([]schema.ContextSection{
		{ID: "mem", File: "AGENTS.md", Files: []string{"DIGITORN.md"}, Priority: 1},
	}, d)
	if !strings.Contains(out, "agents rule") || !strings.Contains(out, "digitorn rule") {
		t.Fatalf("multi-file content missing: %q", out)
	}
	if !strings.Contains(out, "## AGENTS.md") || !strings.Contains(out, "## DIGITORN.md") {
		t.Fatal("multi-file must add per-file headers")
	}
	if !strings.Contains(out, "<system-reminder>") {
		t.Fatal("file section must be wrapped in system-reminder")
	}
}

func TestRender_FileMissingOptional(t *testing.T) {
	d := sampleData()
	d.Session["workdir"] = t.TempDir()
	out := Render([]schema.ContextSection{
		{ID: "mem", File: "AGENTS.md", Optional: true, Priority: 1},
		{Text: "kept"},
	}, d)
	if strings.Contains(out, "AGENTS.md") {
		t.Fatal("optional missing file must be silently skipped")
	}
	if !strings.Contains(out, "kept") {
		t.Fatal("other sections must still render")
	}
}

func TestRender_FileMissingNonOptional(t *testing.T) {
	d := sampleData()
	d.Session["workdir"] = t.TempDir()
	out := Render([]schema.ContextSection{
		{ID: "mem", File: "MISSING.md", Priority: 1},
	}, d)
	if !strings.Contains(out, "MISSING.md") || !strings.Contains(out, "no such file") {
		t.Fatalf("non-optional missing file must emit error marker: %q", out)
	}
}

func TestRender_FileAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "abs.md")
	os.WriteFile(abs, []byte("absolute content"), 0600)
	d := sampleData()
	out := Render([]schema.ContextSection{
		{File: abs},
	}, d)
	if !strings.Contains(out, "absolute content") {
		t.Fatalf("absolute path must work without workdir: %q", out)
	}
}

func TestRender_Dir(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, ".digitorn", "memory")
	os.MkdirAll(memDir, 0755)
	os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte("# Index\n- conventions"), 0600)
	os.WriteFile(filepath.Join(memDir, "conventions.md"), []byte("Always write tests."), 0600)
	os.WriteFile(filepath.Join(memDir, "not-md.txt"), []byte("ignored"), 0600)
	d := sampleData()
	d.Session["workdir"] = dir
	out := Render([]schema.ContextSection{
		{ID: "mem", Dir: ".digitorn/memory", Priority: 1},
	}, d)
	if !strings.Contains(out, "# Index") {
		t.Fatal("MEMORY.md must appear first")
	}
	if !strings.Contains(out, "Always write tests") {
		t.Fatal("other .md files must be included")
	}
	if strings.Contains(out, "ignored") {
		t.Fatal("non-.md files must be excluded")
	}
	if !strings.Contains(out, "<system-reminder>") {
		t.Fatal("dir section must be wrapped in system-reminder")
	}
}

func TestRender_DirOptionalMissing(t *testing.T) {
	d := sampleData()
	d.Session["workdir"] = t.TempDir()
	out := Render([]schema.ContextSection{
		{Dir: ".digitorn/memory", Optional: true},
		{Text: "kept"},
	}, d)
	if strings.Contains(out, "system-reminder") {
		t.Fatal("empty optional dir must produce no output")
	}
	if !strings.Contains(out, "kept") {
		t.Fatal("other sections must still render")
	}
}

func TestBuiltin_MemoryIndex(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, ".digitorn", "memory")
	os.MkdirAll(memDir, 0755)
	os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte("# Memory Index\n- fact1"), 0600)
	os.WriteFile(filepath.Join(memDir, "fact1.md"), []byte("Always test."), 0600)
	d := sampleData()
	d.Session["workdir"] = dir
	out := Render([]schema.ContextSection{
		{Builtin: "memory_index"},
	}, d)
	if !strings.Contains(out, "Memory Index") || !strings.Contains(out, "Always test") {
		t.Fatalf("memory_index must load .digitorn/memory/: %q", out)
	}
	if !strings.Contains(out, "<system-reminder>") {
		t.Fatal("memory content must be wrapped in system-reminder")
	}
	if !strings.Contains(out, "digitorn-directive") {
		t.Fatal("memory_index must always emit the writing directive")
	}
}

func TestBuiltin_MemoryIndexEmitsDirectiveEvenWhenEmpty(t *testing.T) {
	d := sampleData()
	d.Session["workdir"] = t.TempDir()
	out := Render([]schema.ContextSection{
		{Builtin: "memory_index"},
	}, d)
	if !strings.Contains(out, "digitorn-directive") {
		t.Fatal("directive must be emitted even when .digitorn/memory/ does not exist yet")
	}
	if !strings.Contains(out, "Persistent file memory") {
		t.Fatal("directive must contain memory instructions")
	}
	if strings.Contains(out, "<system-reminder>") {
		t.Fatal("system-reminder must NOT appear when there is no memory content")
	}
}

func TestRender_WritableDir(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, "my-memory")
	os.MkdirAll(memDir, 0755)
	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("keep this"), 0600)
	d := sampleData()
	d.Session["workdir"] = dir
	out := Render([]schema.ContextSection{
		{Dir: "my-memory", Writable: true, Priority: 1},
	}, d)
	if !strings.Contains(out, "keep this") {
		t.Fatal("writable dir must still load file content")
	}
	if !strings.Contains(out, "digitorn-directive") {
		t.Fatal("writable:true must inject writing directive")
	}
	if !strings.Contains(out, "my-memory") {
		t.Fatal("directive must reference the configured directory")
	}
}

func TestRender_WritableFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "notes.md"), []byte("note content"), 0600)
	d := sampleData()
	d.Session["workdir"] = dir
	out := Render([]schema.ContextSection{
		{File: "notes.md", Writable: true},
	}, d)
	if !strings.Contains(out, "note content") {
		t.Fatal("writable file must still load content")
	}
	if !strings.Contains(out, "digitorn-directive") {
		t.Fatal("writable:true must inject writing directive")
	}
}

func TestRender_NotWritableNoDirective(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("read only"), 0600)
	d := sampleData()
	d.Session["workdir"] = dir
	out := Render([]schema.ContextSection{
		{File: "AGENTS.md"},
	}, d)
	if strings.Contains(out, "digitorn-directive") {
		t.Fatal("read-only file section must NOT inject writing directive")
	}
}

func TestRender_StaticNotWrapped(t *testing.T) {
	out := Render([]schema.ContextSection{
		{Text: "hardcoded instruction"},
		{Template: "user is {{user.name}}"},
		{Builtin: "datetime"},
	}, sampleData())
	if strings.Contains(out, "<system-reminder>") {
		t.Fatal("static text/template/builtin sections must NOT be wrapped in system-reminder")
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
