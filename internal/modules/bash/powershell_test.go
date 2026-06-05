//go:build windows

package bash

import (
	"context"
	"strings"
	"testing"
	"time"
)

func psShell(t *testing.T) *shell {
	t.Helper()
	kind, path, err := detectShell("powershell")
	if err != nil || kind != "powershell" {
		t.Skip("no PowerShell available")
	}
	sh, err := newShell(kind, path, t.TempDir(), buildEnv(nil), 1<<20)
	if err != nil {
		t.Fatalf("newShell(powershell): %v", err)
	}
	t.Cleanup(sh.close)
	return sh
}

func testModulePS(t *testing.T) *Module {
	t.Helper()
	// PowerShell is no longer a backend: a host without real bash now runs the
	// built-in Go interpreter, never PowerShell (the bash→PS translation was the
	// entire class of bugs we removed). These legacy PS tests are retained for
	// history but no longer exercise a live path.
	t.Skip("PowerShell backend removed; the bash tool uses the built-in Go interpreter")
	m := New()
	if err := m.Init(context.Background(), map[string]any{"workdir": t.TempDir(), "shell": "powershell"}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if m.path == "" || m.kind != "powershell" {
		t.Skip("no PowerShell available")
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	return m
}

func TestPowerShell_PersistsStateAndExitCode(t *testing.T) {
	sh := psShell(t)

	// state persists across calls (the headline "cmd persistant")
	run(t, sh, "$Probe = 42", 15*time.Second)
	res := run(t, sh, "Write-Output \"P=$Probe\"", 15*time.Second)
	if !strings.Contains(res.Stdout, "P=42") {
		t.Fatalf("PowerShell state did not persist: %q", res.Stdout)
	}

	// native exit code is captured
	res = run(t, sh, "cmd /c exit 5", 15*time.Second)
	if res.ExitCode != 5 {
		t.Fatalf("native exit code wrong: got %d want 5 (stdout=%q)", res.ExitCode, res.Stdout)
	}

	// success path reports 0
	res = run(t, sh, "Write-Output ok", 15*time.Second)
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "ok" {
		t.Fatalf("success path wrong: exit=%d stdout=%q", res.ExitCode, res.Stdout)
	}
}

func TestPowerShell_PersistsCwd(t *testing.T) {
	sh := psShell(t)
	run(t, sh, "New-Item -ItemType Directory -Force psproj | Out-Null; Set-Location psproj", 15*time.Second)
	res := run(t, sh, "Split-Path -Leaf $PWD.Path", 15*time.Second)
	if strings.TrimSpace(res.Stdout) != "psproj" {
		t.Fatalf("cwd did not persist: stdout=%q cwd=%q", res.Stdout, res.Cwd)
	}
}

// TestPowerShell_CapturesNativeStderr pins the bug that made the agent loop:
// a failing NATIVE command (npx create-react-app, npm, git) exits non-zero with
// its real reason on stderr. PowerShell does not merge a native command's stderr
// through Invoke-Expression, so the LLM saw "exit code 1" with NO text and kept
// retrying blind. The frame must capture that stderr (merged into output) so the
// model can read the reason and pivot.
func TestPowerShell_CapturesNativeStderr(t *testing.T) {
	sh := psShell(t)
	res := run(t, sh, `cmd /c "echo OUTLINE & echo BOOM-on-stderr 1>&2 & exit 3"`, 15*time.Second)
	if res.ExitCode != 3 {
		t.Fatalf("native exit code wrong: got %d want 3", res.ExitCode)
	}
	combined := res.Stdout + res.Stderr
	if !strings.Contains(combined, "BOOM-on-stderr") {
		t.Fatalf("native stderr was dropped — the LLM would be blind to the failure reason: stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
	if !strings.Contains(combined, "OUTLINE") {
		t.Fatalf("native stdout missing: %q", res.Stdout)
	}
}

// TestPowerShell_AmpAmpChaining proves bash-style `&&` works through the module
// on PowerShell 5.1 (where it is otherwise a parse error) via psChain: the
// second command must run ONLY after the first succeeds, in the same shell.
func TestPowerShell_AmpAmpChaining(t *testing.T) {
	m := testModulePS(t)
	const sess = "amp"
	// `&&` chain: make a dir then enter it — both must take effect.
	rr := invoke(t, m, sess, "New-Item -ItemType Directory -Force chained | Out-Null && Set-Location chained && Write-Output DONE")
	if rr.ExitCode != 0 {
		t.Fatalf("&& chain failed: exit=%d stdout=%q stderr=%q", rr.ExitCode, rr.Stdout, rr.Stderr)
	}
	if !strings.Contains(rr.Stdout, "DONE") {
		t.Fatalf("chained command did not reach the end: %q", rr.Stdout)
	}
}

// TestPowerShell_CoreutilsUnshadowed proves the headline fix for the agent's
// real failures: on Windows the built-in PowerShell aliases (rm→Remove-Item,
// ls→Get-ChildItem, cat→Get-Content, cp, mv) take different flags than the LLM
// emits, so `rm -rf`, `ls -la`, `cat f | head` died with "A parameter cannot be
// found that matches parameter name 'rf'". After the startup unshadow the
// agent's Unix muscle-memory resolves to the real Git coreutils and just works.
// Driven through the FULL module path (run → psChain → anchorCmd → frame).
func TestPowerShell_CoreutilsUnshadowed(t *testing.T) {
	m := testModulePS(t)
	const sess = "coreutils"

	step := func(label, command string, wantOut string) {
		t.Helper()
		rr := invoke(t, m, sess, command)
		if rr.ExitCode != 0 {
			t.Fatalf("%s: exit=%d stdout=%q stderr=%q (bash muscle-memory still broken)", label, rr.ExitCode, rr.Stdout, rr.Stderr)
		}
		if wantOut != "" && !strings.Contains(rr.Stdout+rr.Stderr, wantOut) {
			t.Fatalf("%s: missing %q in output: stdout=%q stderr=%q", label, wantOut, rr.Stdout, rr.Stderr)
		}
	}

	// Prove the unshadowed coreutils (rm -rf, cp, mv) resolve to the real Git
	// binaries — not the PowerShell aliases that reject `-rf`. Each call anchors
	// at root via reset-cwd, so the setup is chained.
	step("setup", `mkdir -p proj && printf 'x' > proj/a.txt`, "")
	step("cp", "cp proj/a.txt proj/b.txt", "") // cp.exe, not Copy-Item
	step("mv", "mv proj/b.txt proj/c.txt", "") // mv.exe, not Move-Item
	step("rm -rf", "rm -rf proj", "")          // rm.exe with -rf, not Remove-Item
	if rr := invoke(t, m, sess, "Test-Path proj"); strings.Contains(rr.Stdout, "True") {
		t.Fatalf("rm -rf did not delete the tree: %q", rr.Stdout)
	}
}

func TestPowerShell_TimeoutKillsTree(t *testing.T) {
	sh := psShell(t)
	res, err := sh.run(context.Background(), "Start-Sleep -Seconds 30", 1*time.Second)
	if err != errTimeout || !res.TimedOut {
		t.Fatalf("expected timeout, got err=%v res=%+v", err, res)
	}
	if !sh.isClosed() {
		t.Fatal("shell not closed after timeout")
	}
}
