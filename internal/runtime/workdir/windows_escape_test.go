//go:build windows

package workdir

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsPathEscape_Battery(t *testing.T) {
	root := t.TempDir()
	pp := NewPolicy(Options{Root: root, Home: t.TempDir()})

	winDir := `C:\Windows\win.ini`

	cases := []string{
		`C:Windows\win.ini`,
		`C:..\..\..\..\Windows\win.ini`,
		`\Windows\win.ini`,
		`\\?\C:\Windows\win.ini`,
		`\\127.0.0.1\C$\Windows\win.ini`,
		`..\..\..\..\..\Windows\win.ini`,
		`..\..\..\..\..\..\..\..\Windows\win.ini`,
		`....\\....\\Windows`,
		`foo\..\..\..\Windows\win.ini`,
		`C:\Windows\win.ini`,
		`/Windows/win.ini`,
		`C:/Windows/win.ini`,
	}

	for _, raw := range cases {
		got, err := pp.Enforce(raw)
		if err != nil {
			continue
		}
		if !within(root, got) {
			t.Errorf("ESCAPE: Enforce(%q) = %q, which is OUTSIDE root %q", raw, got, root)
		}
		if strings.EqualFold(filepath.Clean(got), filepath.Clean(winDir)) {
			t.Errorf("ESCAPE: Enforce(%q) resolved to the real system file %q", raw, got)
		}
	}
}

func TestWindowsPathEscape_DeniedSecretAcrossForms(t *testing.T) {
	home := t.TempDir()
	secret := filepath.Join(home, ".digitorn", "credentials.json")

	pu := NewPolicy(Options{Root: t.TempDir(), Home: home, Unrestricted: true})
	if _, err := pu.Enforce(secret); err == nil {
		t.Fatalf("daemon secret %q reachable under unrestricted policy", secret)
	}
	if _, err := pu.Enforce(filepath.Join(home, ".digitorn", "credentials.json")); err == nil {
		t.Fatalf("daemon secret reachable")
	}
}
