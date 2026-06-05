//go:build windows

package bash

import "testing"

// TestWindowsBash_NeverWSLStub : on Windows, bash resolution must never hand
// back the WSL launcher (System32\bash.exe), which aborts with
// "execvpe(/bin/bash) failed" when no WSL distro is installed. It must find a
// real Git-for-Windows bash, or fall through (so detection picks PowerShell).
func TestWindowsBash_NeverWSLStub(t *testing.T) {
	for _, p := range []string{
		`C:\Windows\System32\bash.exe`,
		`c:\windows\system32\BASH.EXE`,
		`C:\Windows\Sysnative\bash.exe`,
	} {
		if !isWSLBash(p) {
			t.Errorf("isWSLBash(%q) = false, want true (WSL launcher)", p)
		}
	}
	for _, p := range []string{
		`C:\Program Files\Git\usr\bin\bash.exe`,
		`C:\Program Files\Git\bin\bash.exe`,
	} {
		if isWSLBash(p) {
			t.Errorf("isWSLBash(%q) = true, want false (real Git bash)", p)
		}
	}

	// The resolver must never return the WSL stub.
	if p := lookShell("bash"); p != "" && isWSLBash(p) {
		t.Fatalf("lookShell(\"bash\") returned the WSL stub: %q", p)
	}
	// detectShell preferring bash must resolve to a REAL bash (or fall back to
	// another shell) — never the WSL stub.
	kind, path, err := detectShell("bash")
	if err == nil && kind == "bash" && isWSLBash(path) {
		t.Fatalf("detectShell picked the WSL stub: %q", path)
	}
}
