//go:build windows

package bash

import (
	"context"
	"os/exec"
	"testing"
)

// TestWindows_DefaultsToPowerShell locks in the decision: on Windows the
// agent's shell is the system PowerShell (5.1 → pwsh → mvdan fallback). Git
// Bash is NOT picked up automatically — its MSYS layer rewrites native flags
// (`taskkill /F` becomes `F:/`) and isn't guaranteed installed. PowerShell is
// always present on Windows 10+, and the module's translation layer handles
// the common bash idioms (psChain/psEnv/psNulSink/warmupCmd).
func TestWindows_DefaultsToPowerShell(t *testing.T) {
	m := New()
	m.cfg.Workdir = t.TempDir()
	if err := m.Init(context.Background(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	if m.useGoShell {
		t.Fatalf("Windows default must NOT be goshell — that path is opt-in only")
	}
	if m.kind != "powershell" && m.kind != "pwsh" {
		t.Fatalf("Windows default must be powershell/pwsh, got kind=%q useMvdan=%v", m.kind, m.useMvdanSh)
	}
	if m.path == "" {
		t.Fatalf("Windows powershell default must bind a host shell path, got empty")
	}
}

// TestWindows_ExplicitOverrideHonored proves the escape hatch still works: point
// cfg.Shell at a real bash and we use it verbatim.
func TestWindows_ExplicitOverrideHonored(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on PATH to exercise the override")
	}
	m := New()
	m.cfg.Workdir = t.TempDir()
	m.cfg.Shell = bash
	if err := m.Init(context.Background(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	if m.useGoShell {
		t.Fatalf("explicit shell override should bypass goshell (got goshell)")
	}
}
