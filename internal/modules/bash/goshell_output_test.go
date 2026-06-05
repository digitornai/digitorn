package bash

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestGoShell_AlwaysReturnsOutput proves the goshell backend never leaves the
// agent blind: a huge output is truncated WITH a visible marker (not silently
// cut), an empty-output command still reports success, and stderr is surfaced.
func TestGoShell_AlwaysReturnsOutput(t *testing.T) {
	m := New()
	m.cfg.Workdir = t.TempDir()
	if err := m.Init(context.Background(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	m.useGoShell = true
	m.cfg.MaxOutput = 100 // tiny cap so a normal loop overflows it

	get := func(cmd string) (bool, string, string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"command": cmd})
		res, err := m.run(context.Background(), raw)
		if err != nil {
			t.Fatalf("%q: err %v", cmd, err)
		}
		var d struct {
			Stdout string `json:"stdout"`
			Stderr string `json:"stderr"`
		}
		data, _ := json.Marshal(res.Data)
		_ = json.Unmarshal(data, &d)
		return res.Success, d.Stdout, d.Stderr
	}

	// 1. Large output → truncated, but WITH the marker (never silent).
	ok, out, _ := get(`i=0; while [ $i -lt 100 ]; do echo 0123456789; i=$((i+1)); done`)
	if !ok {
		t.Fatalf("large-output command should succeed")
	}
	if !strings.Contains(out, "[truncated ") {
		t.Fatalf("large output silently cut — no truncation marker:\n%q", out)
	}
	if len(out) > 100+40 { // cap + a short marker line, nothing more
		t.Fatalf("buffer not bounded: %d bytes", len(out))
	}

	// 2. Empty-output command still reports success (agent sees exit 0, not a
	// phantom failure).
	ok, out, _ = get(`cd . ; export FOO=bar`)
	if !ok || strings.TrimSpace(out) != "" {
		t.Fatalf("empty-output command: success=%v out=%q (want success, empty)", ok, out)
	}

	// 3. A command that writes to stderr surfaces it.
	_, _, errOut := get(`echo oops 1>&2`)
	if strings.TrimSpace(errOut) != "oops" {
		t.Fatalf("stderr not surfaced: %q", errOut)
	}
}
