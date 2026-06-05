//go:build windows

package bash

import (
	"context"
	"os/exec"
	"testing"
)

// TestWindows_DefaultsToGoShell locks in the decision: on Windows the agent's
// shell is our self-contained goshell, NOT whatever Git Bash happens to be
// installed — so the MSYS argument-rewriting footguns (`taskkill /F` → `F:/`)
// can never come back, and behaviour doesn't drift per machine.
func TestWindows_DefaultsToGoShell(t *testing.T) {
	m := New()
	m.cfg.Workdir = t.TempDir()
	if err := m.Init(context.Background(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	if !m.useGoShell {
		t.Fatalf("Windows default must be goshell, not a host shell")
	}
	if m.path != "" {
		t.Fatalf("must not bind a host shell path on Windows, got %q", m.path)
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
