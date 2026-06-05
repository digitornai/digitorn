//go:build windows

package bash

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestGoShellBusyboxThroughModule is the combined, uncontaminated proof: a
// COMPLEX agent pipeline (sort/uniq/awk/tr/sed/grep, then find/xargs/wc) flows
// through the REAL module path — params → run → goshell → busybox → result —
// on a PATH with NO GNU coreutils (every git dir stripped). It proves the
// agent's shell works on a Windows host that has no bash, end to end at the
// module boundary, not just in goshell unit tests.
func TestGoShellBusyboxThroughModule(t *testing.T) {
	t.Setenv("PATH", stripGit(os.Getenv("PATH")))

	m := New()
	m.cfg.Workdir = t.TempDir()
	if err := m.Init(context.Background(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	m.useGoShell = true

	// The file-op guard intentionally blocks a command that LEADS with find/ls/
	// cat/grep/… (the agent must use the sandboxed filesystem tools for those).
	// So every pipeline here leads with an allowed verb (printf) while still
	// exercising the busybox coreutils mid-pipe.
	cases := []struct{ cmd, want string }{
		// sort -n / uniq / awk -F / tr / sed -n / grep -E, all from busybox.
		{`printf '3:charlie\n1:alpha\n2:bravo\n2:bravo\n' | sort -t: -k1 -n | uniq | awk -F: '{print $2}' | tr a-z A-Z | sed -n '1,2p' | grep -E 'A|B' | tr '\n' ' '`, "ALPHA BRAVO"},
		// xargs grouping (sub-exec) + wc.
		{`printf '%s\n' a b c d | xargs -n2 | wc -l | tr -d ' '`, "2"},
		// uniq -c frequency + reverse numeric sort.
		{`printf 'x\ny\nz\nx\n' | sort | uniq -c | sort -rn | head -1 | awk '{print $2}'`, "x"},
	}
	for _, c := range cases {
		raw, _ := json.Marshal(map[string]any{"command": c.cmd})
		res, err := m.run(context.Background(), raw)
		if err != nil {
			t.Fatalf("%q: err %v", c.cmd, err)
		}
		var d struct {
			Stdout   string `json:"stdout"`
			Stderr   string `json:"stderr"`
			ExitCode int    `json:"exit_code"`
		}
		data, _ := json.Marshal(res.Data)
		_ = json.Unmarshal(data, &d)
		if !res.Success {
			t.Fatalf("%q: not success err=%q | exit=%d stdout=%q stderr=%q", c.cmd, res.Error, d.ExitCode, d.Stdout, d.Stderr)
		}
		if strings.TrimSpace(d.Stdout) != c.want {
			t.Fatalf("%q: stdout=%q want %q (stderr=%q)", c.cmd, d.Stdout, c.want, d.Stderr)
		}
	}
}

// stripGit drops every PATH entry that looks like a Git / MSYS install, so the
// test sees a Windows host with no GNU coreutils — exactly the case busybox
// must cover.
func stripGit(path string) string {
	var keep []string
	for _, p := range strings.Split(path, string(os.PathListSeparator)) {
		low := strings.ToLower(p)
		if strings.Contains(low, "git") || strings.Contains(low, "usr\\bin") || strings.Contains(low, "mingw") {
			continue
		}
		keep = append(keep, p)
	}
	return strings.Join(keep, string(os.PathListSeparator))
}
