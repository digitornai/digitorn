package bash

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestAgentReceivesCommandErrors(t *testing.T) {
	m := newGoShellModule(t)

	script :=`printf 'npm error gyp ERR! build error\nnpm error gyp ERR! not ok\nError: MODULE_NOT_FOUND\n' 1>&2; exit 1`
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

	for _, want := range []string{"npm error gyp ERR! build error", "npm error gyp ERR! not ok", "MODULE_NOT_FOUND"} {
		if !strings.Contains(d.Stderr, want) {
			t.Fatalf("agent did not receive stderr line %q; got:\n%s", want, d.Stderr)
		}
	}
	if d.ExitCode != 1 {
		t.Fatalf("agent got exit_code=%d, want 1", d.ExitCode)
	}
	if !strings.Contains(res.Error, "MODULE_NOT_FOUND") {
		t.Fatalf("failure reason not surfaced in error summary: %q", res.Error)
	}
}
