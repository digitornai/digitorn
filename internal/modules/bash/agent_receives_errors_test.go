package bash

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestAgentReceivesCommandErrors answers a direct question: when a real tool
// (npm, node-gyp, node) fails with a multi-line error and a non-zero exit, does
// the AGENT actually receive that error — or does the shell swallow/hide it?
//
// It reproduces the exact shape of the better-sqlite3 / MODULE_NOT_FOUND failure
// (a stack on stderr, then exit 1) and asserts the agent gets, through the
// module's own run() path:
//   - Success == false,
//   - the FULL stderr (every line) in the result data,
//   - the real exit code,
//   - and the failure REASON surfaced in the error summary (so a weak model
//     can't miss it).
func TestAgentReceivesCommandErrors(t *testing.T) {
	m := newGoShellModule(t)

	// Mimic `npm install better-sqlite3` blowing up in node-gyp: several lines to
	// stderr, a recognizable code, then a non-zero exit.
	script := `printf 'npm error gyp ERR! build error\nnpm error gyp ERR! not ok\nError: MODULE_NOT_FOUND\n' 1>&2; exit 1`
	raw, _ := json.Marshal(map[string]any{"command": script})

	res, err := m.run(context.Background(), raw)
	if err != nil {
		t.Fatalf("run returned a transport error (should be nil; the failure is data): %v", err)
	}
	if res.Success {
		t.Fatalf("a command that exited 1 was reported as success")
	}

	var d runResult
	data, _ := json.Marshal(res.Data)
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The agent receives the full stderr, every line.
	for _, want := range []string{"npm error gyp ERR! build error", "npm error gyp ERR! not ok", "MODULE_NOT_FOUND"} {
		if !strings.Contains(d.Stderr, want) {
			t.Fatalf("agent did not receive stderr line %q; got:\n%s", want, d.Stderr)
		}
	}
	if d.ExitCode != 1 {
		t.Fatalf("agent got exit_code=%d, want 1", d.ExitCode)
	}
	// And the reason is hoisted into the error summary the model reads first.
	if !strings.Contains(res.Error, "MODULE_NOT_FOUND") {
		t.Fatalf("failure reason not surfaced in error summary: %q", res.Error)
	}
}
