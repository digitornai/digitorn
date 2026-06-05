package workdir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolve_UserWorkdir_UsedAsIs(t *testing.T) {
	uw := t.TempDir()
	got, err := Resolve(Request{Mode: ModeAuto, UserWorkdir: uw, Home: t.TempDir(),
		AppID: "app", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatalf("user workdir should resolve: %v", err)
	}
	if got != canonical(uw) {
		t.Errorf("got %q, want %q", got, canonical(uw))
	}
	// No managed dir should have been created under home for this case.
}

func TestResolve_UserWorkdir_Invalid(t *testing.T) {
	// Not absolute → error.
	if _, err := Resolve(Request{UserWorkdir: "relative/dir", Home: t.TempDir()}); err == nil {
		t.Error("relative user workdir must error")
	}
	// Absolute but a file, not a dir → error (MkdirAll fails on a file path).
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(Request{UserWorkdir: f, Home: t.TempDir()}); err == nil {
		t.Error("file-as-workdir must error")
	}
}

func TestResolve_UserWorkdir_MissingIsCreated(t *testing.T) {
	// Absolute but not yet existing → created (like fixed mode).
	missing := filepath.Join(t.TempDir(), "new", "proj")
	got, err := Resolve(Request{UserWorkdir: missing, Home: t.TempDir()})
	if err != nil {
		t.Fatalf("missing user workdir should be created: %v", err)
	}
	if info, err := os.Stat(got); err != nil || !info.IsDir() {
		t.Errorf("user workdir must exist as a dir after resolve: got=%q err=%v", got, err)
	}
}

func TestResolve_Required_NoWorkdir_Errors(t *testing.T) {
	_, err := Resolve(Request{Mode: ModeRequired, Home: t.TempDir(),
		AppID: "app", UserID: "u", SessionID: "s"})
	if !errors.Is(err, ErrWorkdirRequired) {
		t.Fatalf("required mode with no workdir must return ErrWorkdirRequired, got %v", err)
	}
}

func TestResolve_Required_WithUserWorkdir_OK(t *testing.T) {
	uw := t.TempDir()
	got, err := Resolve(Request{Mode: ModeRequired, UserWorkdir: uw, Home: t.TempDir()})
	if err != nil {
		t.Fatalf("required + supplied workdir must resolve: %v", err)
	}
	if got != canonical(uw) {
		t.Errorf("got %q, want %q", got, canonical(uw))
	}
}

func TestResolve_Fixed_CreatesAndReturns(t *testing.T) {
	base := t.TempDir()
	fixed := filepath.Join(base, "pinned", "proj")
	got, err := Resolve(Request{Mode: ModeFixed, FixedPath: fixed, Home: t.TempDir()})
	if err != nil {
		t.Fatalf("fixed must resolve: %v", err)
	}
	if info, err := os.Stat(got); err != nil || !info.IsDir() {
		t.Errorf("fixed workdir should exist as a dir: got=%q err=%v", got, err)
	}
}

func TestResolve_Fixed_EmptyPath_Errors(t *testing.T) {
	if _, err := Resolve(Request{Mode: ModeFixed, Home: t.TempDir()}); err == nil {
		t.Error("fixed mode with empty path must error")
	}
}

func TestResolve_None_ReturnsEmpty(t *testing.T) {
	got, err := Resolve(Request{Mode: ModeNone, Home: t.TempDir(),
		AppID: "app", UserID: "u", SessionID: "s"})
	if err != nil || got != "" {
		t.Errorf("none mode must return empty, got=%q err=%v", got, err)
	}
}

func TestResolve_Auto_CreatesManagedPerSessionDir(t *testing.T) {
	home := t.TempDir()
	got, err := Resolve(Request{Mode: ModeAuto, Home: home,
		AppID: "myapp", UserID: "alice", SessionID: "sess-123"})
	if err != nil {
		t.Fatalf("auto must resolve: %v", err)
	}
	if info, err := os.Stat(got); err != nil || !info.IsDir() {
		t.Fatalf("managed workdir must exist: got=%q err=%v", got, err)
	}
	wantSuffix := filepath.Join("workdirs", "myapp", "alice", "sess-123")
	if !strings.Contains(got, wantSuffix) {
		t.Errorf("managed path %q must contain %q", got, wantSuffix)
	}
}

func TestResolve_Auto_EmptyMode_DefaultsToManaged(t *testing.T) {
	home := t.TempDir()
	got, err := Resolve(Request{Mode: NormalizeMode(""), Home: home, AppID: "a", UserID: "u", SessionID: "s"})
	if err != nil || got == "" {
		t.Fatalf("empty mode should default to a managed workdir, got=%q err=%v", got, err)
	}
}

func TestResolve_Auto_HostileIdsCannotTraverse(t *testing.T) {
	home := t.TempDir()
	got, err := Resolve(Request{Mode: ModeAuto, Home: home,
		AppID: "../../../etc", UserID: "..", SessionID: "a/b"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// The result must stay under <home>/.digitorn/workdirs — no traversal out.
	base := canonical(filepath.Join(home, ".digitorn", "workdirs"))
	if !within(base, got) {
		t.Errorf("hostile ids escaped the workdirs tree: got=%q base=%q", got, base)
	}
}

func TestNormalizeMode(t *testing.T) {
	cases := map[string]Mode{"": ModeAuto, "auto": ModeAuto, "none": ModeNone,
		"fixed": ModeFixed, "required": ModeRequired, "bogus": ModeAuto}
	for in, want := range cases {
		if got := NormalizeMode(in); got != want {
			t.Errorf("NormalizeMode(%q)=%q want %q", in, got, want)
		}
	}
}
