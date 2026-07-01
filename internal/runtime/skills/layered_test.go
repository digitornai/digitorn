package skills_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/context/meta"
	"github.com/digitornai/digitorn/internal/runtime/skills"
)

// userLayer is a controllable UserLoader for the layered tests.
type userLayer struct {
	entry meta.SkillEntry
	found bool
	err   error
	calls int
}

func (u *userLayer) Load(_ context.Context, _, _, _ string) (meta.SkillEntry, bool, error) {
	u.calls++
	return u.entry, u.found, u.err
}

// bundleWith builds a BundleLoader backed by one app that declares one skill.
func bundleWith(t *testing.T) *skills.BundleLoader {
	t.Helper()
	app := &appmgr.RuntimeApp{
		BundleDir: t.TempDir(),
		Definition: &schema.AppDefinition{
			Dev: &schema.DevBlock{Skills: []schema.SkillEntry{
				{Command: "/commit", Description: "app commit", Path: "commit.md"},
			}},
		},
	}
	// Write the skill file so the bundle loader can read it.
	if err := os.WriteFile(filepath.Join(app.BundleDir, "commit.md"), []byte("APP COMMIT BODY"), 0o644); err != nil {
		t.Fatal(err)
	}
	return skills.New(&fakeApps{apps: map[string]*appmgr.RuntimeApp{"app1": app}})
}

func TestLayered_UserWinsOverApp(t *testing.T) {
	user := &userLayer{entry: meta.SkillEntry{Command: "/commit", Content: "USER COMMIT"}, found: true}
	l := skills.NewLayered(user, bundleWith(t))

	got, err := l.Load(context.Background(), "app1", "user-A", "/commit")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Content != "USER COMMIT" {
		t.Fatalf("expected user skill to win, got %q", got.Content)
	}
}

func TestLayered_FallsThroughToApp(t *testing.T) {
	user := &userLayer{found: false} // user has no such skill
	l := skills.NewLayered(user, bundleWith(t))

	got, err := l.Load(context.Background(), "app1", "user-A", "/commit")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Content != "APP COMMIT BODY" {
		t.Fatalf("expected app skill fallback, got %q", got.Content)
	}
	if user.calls != 1 {
		t.Fatalf("user layer should have been consulted once, got %d", user.calls)
	}
}

func TestLayered_UserErrorFallsThrough(t *testing.T) {
	user := &userLayer{err: errors.New("db down")}
	l := skills.NewLayered(user, bundleWith(t))

	// A user-layer error must NOT hide a good bundled skill.
	got, err := l.Load(context.Background(), "app1", "user-A", "/commit")
	if err != nil || got.Content != "APP COMMIT BODY" {
		t.Fatalf("expected app fallback on user error, got content=%q err=%v", got.Content, err)
	}
}

func TestLayered_AnonymousSkipsUserLayer(t *testing.T) {
	user := &userLayer{entry: meta.SkillEntry{Content: "USER"}, found: true}
	l := skills.NewLayered(user, bundleWith(t))

	// Empty userID → user layer is skipped entirely (app skills only).
	got, _ := l.Load(context.Background(), "app1", "", "/commit")
	if got.Content != "APP COMMIT BODY" {
		t.Fatalf("anonymous should hit app skills, got %q", got.Content)
	}
	if user.calls != 0 {
		t.Fatalf("user layer must not be consulted for empty userID, got %d", user.calls)
	}
}
