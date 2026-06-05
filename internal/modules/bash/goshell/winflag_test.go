//go:build windows

package goshell

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// TestWinFlag_SlashOption guards a real advantage of the self-contained backend:
// goshell does NOT rewrite a Windows-style slash option like `/F` (taskkill /F,
// robocopy /E) into a drive path `F:/`. A host Git Bash, by contrast, mangles it
// via MSYS arg conversion ("Invalid argument/option - 'F:/'"). So `taskkill /F`
// works under goshell as written.
func TestWinFlag_SlashOption(t *testing.T) {
	run := func(script string) string {
		var out, errb bytes.Buffer
		Run(context.Background(), script, t.TempDir(), os.Environ(), nil, &out, &errb)
		return strings.TrimSpace(out.String())
	}
	cases := map[string]string{
		`echo /F`:          "/F",
		`echo /F /PID 123`: "/F /PID 123",
		`echo "/F"`:        "/F",
		`echo /PID`:        "/PID",
	}
	for script, want := range cases {
		if got := run(script); got != want {
			t.Fatalf("%s -> %q, want %q (goshell must not mangle Windows slash flags)", script, got, want)
		}
	}
}
