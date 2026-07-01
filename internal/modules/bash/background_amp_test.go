package bash

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/modules/bash/goshell"
)

// TestBackgroundAmpGuard proves the trailing-`&` guard: an orphaning background
// launch is rejected and redirected to background_run, while `&&` chains, a
// quoted `&`, and a properly reaped `… & wait` all run normally.
func TestBackgroundAmpGuard(t *testing.T) {
	m := newGoShellModule(t)
	run := func(cmd string) (success bool, errMsg string) {
		raw, _ := json.Marshal(map[string]any{"command": cmd})
		res, err := m.run(context.Background(), raw)
		if err != nil {
			t.Fatalf("%q: transport err %v", cmd, err)
		}
		return res.Success, res.Error
	}

	// Trailing & → rejected, with the background_run redirect.
	if ok, msg := run(`node server.js &`); ok || !strings.Contains(msg, "background_run") {
		t.Fatalf("trailing & must be rejected with a background_run hint; success=%v msg=%q", ok, msg)
	}
	if ok, _ := run(`npm run dev &`); ok {
		t.Fatalf("`npm run dev &` must be rejected")
	}

	// Legitimate commands must NOT be caught.
	if ok, msg := run(`echo a && echo b`); !ok {
		t.Fatalf("&& chain wrongly rejected: %q", msg)
	}
	if ok, msg := run(`echo "a & b"`); !ok {
		t.Fatalf("quoted & wrongly rejected: %q", msg)
	}
	if ok, msg := run(`sleep 0.1 & wait`); !ok {
		t.Fatalf("`& wait` (reaped) wrongly rejected: %q", msg)
	}
}

// TestTrailingBackgroundParse checks the parse-based detector directly.
func TestTrailingBackgroundParse(t *testing.T) {
	cases := map[string]bool{
		`node server.js &`: true,
		`a & b &`:          true, // last stmt also backgrounded
		`a && b`:           false,
		`echo "a & b"`:     false, // & is inside quotes
		`a & wait`:         false, // last stmt is wait
		`echo hi`:          false,
	}
	for script, want := range cases {
		if got := goshell.TrailingBackground(script); got != want {
			t.Fatalf("TrailingBackground(%q)=%v want %v", script, got, want)
		}
	}
}
