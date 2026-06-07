package skills_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/runtime/skills"
)

// =====================================================================
// fakeApps — minimal app manager with a configurable definition.
// =====================================================================

type fakeApps struct{ apps map[string]*appmgr.RuntimeApp }

func (f *fakeApps) Install(context.Context, string, string) (*appmgr.App, error) { return nil, nil }
func (f *fakeApps) Upgrade(context.Context, string, string, string) (*appmgr.App, error) {
	return nil, nil
}
func (f *fakeApps) Uninstall(context.Context, string, bool) error { return nil }
func (f *fakeApps) Enable(context.Context, string) error          { return nil }
func (f *fakeApps) Disable(context.Context, string) error         { return nil }
func (f *fakeApps) SetBYOK(context.Context, string, bool) error   { return nil }
func (f *fakeApps) Reload(context.Context, string) error          { return nil }
func (f *fakeApps) CheckUpdate(context.Context, string, string) (*appmgr.UpdateInfo, error) {
	return nil, nil
}
func (f *fakeApps) List(context.Context, bool) ([]appmgr.App, error)    { return nil, nil }
func (f *fakeApps) ListDisabled(context.Context) ([]appmgr.App, error)  { return nil, nil }
func (f *fakeApps) GetApp(context.Context, string) (*appmgr.App, error) { return nil, nil }
func (f *fakeApps) Get(_ context.Context, appID string) (*appmgr.RuntimeApp, error) {
	if a, ok := f.apps[appID]; ok {
		return a, nil
	}
	return nil, appmgr.ErrAppNotFound
}
func (f *fakeApps) GetManifest(context.Context, string) (*schema.AppDefinition, error) {
	return nil, nil
}
func (f *fakeApps) Bootstrap(context.Context) error { return nil }

// buildApp constructs a RuntimeApp with dev.skills[] and writes
// the corresponding markdown files under bundleDir.
func buildApp(t *testing.T, appID string, entries []schema.SkillEntry, files map[string]string) *appmgr.RuntimeApp {
	t.Helper()
	dir := t.TempDir()
	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &appmgr.RuntimeApp{
		Meta:       &appmgr.App{AppID: appID, Enabled: true},
		BundleDir:  dir,
		Definition: &schema.AppDefinition{Dev: &schema.DevBlock{Skills: entries}},
	}
}

// =====================================================================
// 1. Resolution by command
// =====================================================================

func TestBundleLoader_ResolvesByCommand(t *testing.T) {
	app := buildApp(t, "app1",
		[]schema.SkillEntry{{Command: "/commit", Description: "Stage + commit", Path: "skills/commit.md"}},
		map[string]string{"skills/commit.md": "# Commit workflow\n..."},
	)
	apps := &fakeApps{apps: map[string]*appmgr.RuntimeApp{"app1": app}}
	l := skills.New(apps)

	entry, err := l.Load(context.Background(), "app1", "","/commit")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if entry.Command != "/commit" {
		t.Errorf("command = %q", entry.Command)
	}
	if entry.Description != "Stage + commit" {
		t.Errorf("description = %q", entry.Description)
	}
	if !strings.HasPrefix(entry.Content, "# Commit workflow") {
		t.Errorf("content lost : %q", entry.Content)
	}
}

func TestBundleLoader_AcceptsCommandWithoutSlash(t *testing.T) {
	app := buildApp(t, "app1",
		[]schema.SkillEntry{{Command: "/commit", Path: "skills/commit.md"}},
		map[string]string{"skills/commit.md": "body"},
	)
	apps := &fakeApps{apps: map[string]*appmgr.RuntimeApp{"app1": app}}
	l := skills.New(apps)

	for _, in := range []string{"commit", "/commit", "  commit  ", "/Commit", "COMMIT"} {
		entry, err := l.Load(context.Background(), "app1", "",in)
		if err != nil {
			t.Errorf("input %q : %v", in, err)
		}
		if entry.Command != "/commit" {
			t.Errorf("input %q → command = %q", in, entry.Command)
		}
	}
}

// =====================================================================
// 2. Misses
// =====================================================================

func TestBundleLoader_CommandNotDeclared(t *testing.T) {
	app := buildApp(t, "app1",
		[]schema.SkillEntry{{Command: "/commit", Path: "skills/commit.md"}},
		map[string]string{"skills/commit.md": "x"},
	)
	apps := &fakeApps{apps: map[string]*appmgr.RuntimeApp{"app1": app}}
	l := skills.New(apps)
	if _, err := l.Load(context.Background(), "app1", "","/deploy"); err == nil {
		t.Error("expected not-found")
	}
}

func TestBundleLoader_EmptyCommand(t *testing.T) {
	l := skills.New(&fakeApps{})
	if _, err := l.Load(context.Background(), "any", "", ""); err == nil {
		t.Error("empty command should error")
	}
}

func TestBundleLoader_AppWithoutDevBlock(t *testing.T) {
	app := &appmgr.RuntimeApp{
		Meta:       &appmgr.App{AppID: "app1"},
		Definition: &schema.AppDefinition{}, // no Dev
	}
	apps := &fakeApps{apps: map[string]*appmgr.RuntimeApp{"app1": app}}
	l := skills.New(apps)
	if _, err := l.Load(context.Background(), "app1", "","/commit"); err == nil {
		t.Error("app with no dev block should error")
	}
}

// =====================================================================
// 3. Path traversal protection
// =====================================================================

func TestBundleLoader_RejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	// Create a "secret" file outside the bundle.
	secretParent := filepath.Dir(dir)
	if err := os.WriteFile(filepath.Join(secretParent, "secret.md"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := &appmgr.RuntimeApp{
		Meta:      &appmgr.App{AppID: "app1"},
		BundleDir: dir,
		Definition: &schema.AppDefinition{Dev: &schema.DevBlock{
			Skills: []schema.SkillEntry{
				{Command: "/escape", Path: "../secret.md"},
			},
		}},
	}
	apps := &fakeApps{apps: map[string]*appmgr.RuntimeApp{"app1": app}}
	l := skills.New(apps)
	if _, err := l.Load(context.Background(), "app1", "","/escape"); err == nil {
		t.Error("path traversal must be rejected")
	}
}

// =====================================================================
// 4. Cache
// =====================================================================

func TestBundleLoader_CachesResolvedEntries(t *testing.T) {
	app := buildApp(t, "app1",
		[]schema.SkillEntry{{Command: "/commit", Path: "skills/commit.md"}},
		map[string]string{"skills/commit.md": "v1"},
	)
	apps := &fakeApps{apps: map[string]*appmgr.RuntimeApp{"app1": app}}
	l := skills.New(apps)

	first, _ := l.Load(context.Background(), "app1", "","/commit")
	// Mutate the file on disk ; cache should serve the old content.
	os.WriteFile(filepath.Join(app.BundleDir, "skills", "commit.md"), []byte("v2"), 0o644)
	second, _ := l.Load(context.Background(), "app1", "","/commit")
	if first.Content != "v1" || second.Content != "v1" {
		t.Errorf("cache miss : %q / %q", first.Content, second.Content)
	}
}

// =====================================================================
// 5. Per-app isolation
// =====================================================================

func TestBundleLoader_PerAppIsolation(t *testing.T) {
	appA := buildApp(t, "A",
		[]schema.SkillEntry{{Command: "/commit", Path: "skills/commit.md"}},
		map[string]string{"skills/commit.md": "A"},
	)
	appB := buildApp(t, "B",
		[]schema.SkillEntry{{Command: "/commit", Path: "skills/commit.md"}},
		map[string]string{"skills/commit.md": "B"},
	)
	apps := &fakeApps{apps: map[string]*appmgr.RuntimeApp{"A": appA, "B": appB}}
	l := skills.New(apps)

	a, _ := l.Load(context.Background(), "A", "", "/commit")
	b, _ := l.Load(context.Background(), "B", "", "/commit")
	if a.Content != "A" || b.Content != "B" {
		t.Errorf("isolation broken : %q / %q", a.Content, b.Content)
	}
}

// =====================================================================
// 6. Missing file on disk
// =====================================================================

func TestBundleLoader_MissingFileReportsError(t *testing.T) {
	app := &appmgr.RuntimeApp{
		Meta:      &appmgr.App{AppID: "app1"},
		BundleDir: t.TempDir(),
		Definition: &schema.AppDefinition{Dev: &schema.DevBlock{
			Skills: []schema.SkillEntry{
				{Command: "/missing", Path: "skills/missing.md"},
			},
		}},
	}
	apps := &fakeApps{apps: map[string]*appmgr.RuntimeApp{"app1": app}}
	l := skills.New(apps)
	if _, err := l.Load(context.Background(), "app1", "","/missing"); err == nil {
		t.Error("missing file should error")
	}
}
