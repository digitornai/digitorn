package workdir

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// newTestPolicy builds a policy rooted at a fresh temp workdir, with a fake
// HOME so the daemon-secret denylist points at a sandbox we control.
func newTestPolicy(t *testing.T, unrestricted bool, allowed ...string) (PathPolicy, string, string) {
	t.Helper()
	wd := t.TempDir()
	home := t.TempDir()
	p := NewPolicy(Options{Root: wd, AllowedExtra: allowed, Unrestricted: unrestricted, Home: home})
	return p, p.Root(), home
}

func TestEnforce_RelativeRebased(t *testing.T) {
	p, root, _ := newTestPolicy(t, false)
	got, err := p.Enforce("notes/a.txt")
	if err != nil {
		t.Fatalf("relative inside must be allowed: %v", err)
	}
	want := filepath.Join(root, "notes", "a.txt")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEnforce_EscapesRejected(t *testing.T) {
	p, _, _ := newTestPolicy(t, false)
	for _, bad := range []string{"../escape.txt", "a/../../escape.txt", "../../etc/passwd", ".."} {
		if _, err := p.Enforce(bad); err == nil {
			t.Errorf("escape %q must be rejected", bad)
		}
	}
}

func TestEnforce_AbsoluteInsideAllowed_OutsideRejected(t *testing.T) {
	p, root, _ := newTestPolicy(t, false)
	inside := filepath.Join(root, "sub", "f.txt")
	if got, err := p.Enforce(inside); err != nil || got != inside {
		t.Errorf("absolute inside must be allowed: got=%q err=%v", got, err)
	}
	outside := filepath.Join(t.TempDir(), "other.txt")
	if _, err := p.Enforce(outside); err == nil {
		t.Errorf("absolute outside must be rejected: %q", outside)
	}
}

func TestEnforce_EmptyAndBlank(t *testing.T) {
	p, _, _ := newTestPolicy(t, false)
	if _, err := p.Enforce("   "); err == nil {
		t.Error("blank path must be rejected")
	}
}

func TestEnforce_NoWorkdir_RejectsRelative(t *testing.T) {
	p := NewPolicy(Options{Home: t.TempDir()}) // root == ""
	if p.HasWorkdir() {
		t.Fatal("expected no workdir")
	}
	if _, err := p.Enforce("a.txt"); err == nil {
		t.Error("relative path must be rejected when there is no workdir")
	}
}

func TestEnforce_DaemonSecret_AlwaysDenied_EvenUnrestricted(t *testing.T) {
	wd := t.TempDir()
	home := t.TempDir()
	dg := filepath.Join(home, ".digitorn")
	if err := os.MkdirAll(dg, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dg, "master.key")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionsDir := filepath.Join(dg, "sessions", "s1")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Unrestricted lifts workdir confinement but NEVER the secret denylist.
	p := NewPolicy(Options{Root: wd, Unrestricted: true, Home: home})
	if _, err := p.Enforce(secret); err == nil {
		t.Error("master.key must be denied even when unrestricted")
	}
	if _, err := p.Enforce(filepath.Join(sessionsDir, "evt.jsonl")); err == nil {
		t.Error("a path under ~/.digitorn/sessions must be denied even when unrestricted")
	}
	// A non-secret outside path IS allowed under unrestricted.
	ok := filepath.Join(t.TempDir(), "scratch.txt")
	if _, err := p.Enforce(ok); err != nil {
		t.Errorf("unrestricted should allow a non-secret outside path: %v", err)
	}
}

func TestEnforce_AllowedExtra(t *testing.T) {
	extra := t.TempDir()
	p := NewPolicy(Options{Root: t.TempDir(), AllowedExtra: []string{extra}, Home: t.TempDir()})
	in := filepath.Join(extra, "cache", "x")
	if _, err := p.Enforce(in); err != nil {
		t.Errorf("path under an allowed-extra root must be allowed: %v", err)
	}
	if _, err := p.Enforce(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("a path under neither root must be rejected")
	}
}

func TestEnforce_SymlinkEscapeRejected(t *testing.T) {
	p, root, _ := newTestPolicy(t, false)
	outside := t.TempDir()
	link := filepath.Join(root, "evil")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported on this platform/run: %v", err)
	}
	// Writing "through" the symlink must be caught (resolved before check).
	if _, err := p.Enforce("evil/passwd"); err == nil {
		t.Errorf("write through a symlink that exits the workdir must be rejected")
	}
	_ = runtime.GOOS
}

func TestIsAllowed(t *testing.T) {
	p, _, _ := newTestPolicy(t, false)
	if !p.IsAllowed("ok/file.txt") {
		t.Error("inside path should be allowed")
	}
	if p.IsAllowed("../escape") {
		t.Error("escape should not be allowed")
	}
}
