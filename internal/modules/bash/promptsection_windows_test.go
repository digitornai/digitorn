//go:build windows

package bash

import (
	"context"
	"strings"
	"testing"

	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
)

// TestPromptSection_WindowsDialect verifies the agent is told, up front and
// unambiguously, that it is running PowerShell (not bash) on Windows — and
// receives accurate Windows command guidance instead of Unix-only hints.
func TestPromptSection_WindowsDialect(t *testing.T) {
	m := New()
	m.cfg.Workdir = t.TempDir()
	if err := m.Init(context.Background(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	secs := m.PromptSections(domainmodule.PromptScope{})
	if len(secs) == 0 {
		t.Fatal("no prompt sections")
	}
	body := secs[0].Content
	for _, want := range []string{
		"PowerShell",
		"POWERSHELL COMMANDS",
		"$env:VAR",
		"Get-Process",
		"Get-NetTCPConnection",
		"Remove-Item -Recurse -Force",
		"taskkill /F /PID",
		"netstat -ano",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Windows shell guidance missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "lsof") {
		t.Fatalf("stale Unix-only `lsof` hint still shown on Windows")
	}
	if strings.Contains(body, "BASH-compatible") {
		t.Fatalf("stale 'BASH-compatible' claim still shown on Windows (default is PowerShell)")
	}
}
