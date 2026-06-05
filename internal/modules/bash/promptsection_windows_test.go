//go:build windows

package bash

import (
	"context"
	"strings"
	"testing"

	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
)

// TestPromptSection_WindowsDialect verifies the agent is told, up front and
// unambiguously, that it is on a bash shell (not cmd/PS/MSYS) with the path and
// flag rules — so it stops faceplanting on `cd /d`, `/c/Users`, and unquoted
// backslash paths instead of discovering them one failed command at a time.
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
		"BASH-compatible",
		"NEVER `cd /d`",
		"/c/Users", // tells it MSYS paths don't work
		"FORWARD slashes",
		"taskkill /F /PID", // correct Windows process-kill, not lsof
		"netstat -ano",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Windows shell guidance missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "lsof") {
		t.Fatalf("stale Unix-only `lsof` hint still shown on Windows")
	}
}
