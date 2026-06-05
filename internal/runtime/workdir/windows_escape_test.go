//go:build windows

package workdir

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestWindowsPathEscape_Battery is the security verification for the one class
// the path audit flagged as "possibly critical, untested": Windows path forms
// where filepath.IsAbs is FALSE but the string still carries a volume or root
// (drive-relative "C:foo", rooted "\foo", "..", UNC). Each MUST either be
// rejected OR resolve to a path INSIDE the workdir — never escape it.
func TestWindowsPathEscape_Battery(t *testing.T) {
	root := t.TempDir() // e.g. C:\Users\...\Temp\TestXxx
	pp := NewPolicy(Options{Root: root, Home: t.TempDir()})

	// A path OUTSIDE the root we must never be able to reach.
	winDir := `C:\Windows\win.ini`

	cases := []string{
		`C:Windows\win.ini`,        // drive-relative (IsAbs false)
		`C:..\..\..\..\Windows\win.ini`,
		`\Windows\win.ini`,         // rooted, no drive (IsAbs false)
		`\\?\C:\Windows\win.ini`,   // extended-length UNC
		`\\127.0.0.1\C$\Windows\win.ini`, // UNC admin share
		`..\..\..\..\..\Windows\win.ini`,
		`..\..\..\..\..\..\..\..\Windows\win.ini`,
		`....\\....\\Windows`,
		`foo\..\..\..\Windows\win.ini`,
		`C:\Windows\win.ini`,       // fully-qualified abs (IsAbs true) — must be rejected
		`/Windows/win.ini`,         // forward-slash rooted
		`C:/Windows/win.ini`,
	}

	for _, raw := range cases {
		got, err := pp.Enforce(raw)
		if err != nil {
			continue // rejected — safe
		}
		// Accepted: it MUST be inside the root, and MUST NOT be the real win.ini.
		if !within(root, got) {
			t.Errorf("ESCAPE: Enforce(%q) = %q, which is OUTSIDE root %q", raw, got, root)
		}
		if strings.EqualFold(filepath.Clean(got), filepath.Clean(winDir)) {
			t.Errorf("ESCAPE: Enforce(%q) resolved to the real system file %q", raw, got)
		}
	}
}

// TestWindowsPathEscape_DeniedSecretAcrossForms makes sure the daemon-secret
// denylist (credentials.json under ~/.digitorn) is rejected even under an
// unrestricted policy — the one rule nothing lifts.
func TestWindowsPathEscape_DeniedSecretAcrossForms(t *testing.T) {
	home := t.TempDir()
	secret := filepath.Join(home, ".digitorn", "credentials.json")

	// Even an UNRESTRICTED policy must reject a daemon secret.
	pu := NewPolicy(Options{Root: t.TempDir(), Home: home, Unrestricted: true})
	if _, err := pu.Enforce(secret); err == nil {
		t.Fatalf("daemon secret %q reachable under unrestricted policy", secret)
	}
	// And the kv/sessions/state dirs under it too.
	if _, err := pu.Enforce(filepath.Join(home, ".digitorn", "credentials.json")); err == nil {
		t.Fatalf("daemon secret reachable")
	}
}
