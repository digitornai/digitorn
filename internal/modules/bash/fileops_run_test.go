package bash

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestFileOpsRunOnShell proves the file-op guard is gone: ls/cat/grep/find and a
// heredoc now RUN on the shell (goshell handles them all) instead of being
// failed with a "use filesystem.glob" redirect. The shell behaves like a shell.
func TestFileOpsRunOnShell(t *testing.T) {
	m := newGoShellModule(t)
	dir := m.cfg.Workdir
	if err := os.WriteFile(dir+"/a.txt", []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(cmd string) (bool, string) {
		raw, _ := json.Marshal(map[string]any{"command": cmd})
		res, err := m.run(context.Background(), raw)
		if err != nil {
			t.Fatalf("%q: %v", cmd, err)
		}
		var d runResult
		b, _ := json.Marshal(res.Data)
		_ = json.Unmarshal(b, &d)
		return res.Success, d.Stdout
	}

	cases := []struct{ cmd, want string }{
		{`ls`, "a.txt"},
		{`cat a.txt`, "hello"},
		{`grep world a.txt`, "world"},
		{`find . -name '*.txt'`, "a.txt"},
		{"cat <<EOF\nfromheredoc\nEOF", "fromheredoc"},
	}
	for _, c := range cases {
		ok, out := run(c.cmd)
		if !ok {
			t.Fatalf("%q: should run on the shell, but failed (out=%q)", c.cmd, out)
		}
		if !strings.Contains(out, c.want) {
			t.Fatalf("%q: stdout %q missing %q", c.cmd, out, c.want)
		}
	}
}
