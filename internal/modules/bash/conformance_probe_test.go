//go:build windows

package bash

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
)

// TestProbe_BashMuscleMemory surveys the bash-style commands an LLM agent emits
// by reflex and logs, for each, what the agent actually receives. Logging only
// (no assertions) — its job is to produce the conformance gap list, not gate CI.
// Run: go test ./internal/modules/bash/ -run BashMuscleMemory -v
func TestProbe_BashMuscleMemory(t *testing.T) {
	m := testModulePS(t)
	probe := func(cmd string) (runResult, string) {
		ctx := tool.WithIdentity(context.Background(), tool.Identity{AppID: "app", SessionID: "probe"})
		raw, _ := json.Marshal(runParams{Command: cmd})
		res, _ := m.run(ctx, raw)
		rr, _ := res.Data.(runResult)
		return rr, res.Error
	}
	clip := func(s string) string {
		s = strings.TrimSpace(s)
		if len(s) > 160 {
			s = s[:160] + "…"
		}
		return s
	}

	cases := []struct{ name, cmd string }{
		// --- bash null redirects (the dominant reflex) ---
		{"bash 2>/dev/null", `node --version 2>/dev/null`},
		{"bash >/dev/null", `node --version >/dev/null`},
		{"bash >/dev/null 2>&1", `node --version >/dev/null 2>&1`},
		{"bash redirect to /dev/null only", `echo hi 2>/dev/null`},
		// --- env / vars ---
		{"export then use", `export FOO=bar; echo $FOO`},
		{"echo $HOME", `echo $HOME`},
		{"echo $PWD", `echo $PWD`},
		{"inline env var", `FOO=bar node -e "console.log(process.env.FOO)"`},
		// --- common coreutils the agent reaches for ---
		{"which", `which node`},
		{"touch", `touch __probe_touch.txt`},
		{"command -v", `command -v node`},
		{"env", `env`},
		// --- command substitution ---
		{"dollar-paren subst", `echo "node=$(node --version)"`},
		{"backtick subst", "echo \"node=`node --version`\""},
		// --- test / conditionals / loops (bash) ---
		{"test -f &&", `test -f go.mod && echo HASMOD`},
		{"[ -d ] &&", `[ -d . ] && echo ISDIR`},
		{"for loop", `for f in a b c; do echo $f; done`},
		{"if/then/fi", `if [ -d . ]; then echo yes; fi`},
		// --- write + read round trips ---
		{"echo > file", `echo hello > __probe_w.txt`},
		{"printf", `printf 'x=%s\n' hi`},
		// --- chaining + success path ---
		{"&& chain ok", `node --version && echo CHAINOK`},
		{"|| fallback", `false || echo FELLBACK`},
		{"semicolon seq", `echo one; echo two`},
		// --- venv activate (bash) ---
		{"source activate", `source venv/bin/activate`},
		// --- python (PATH fix) ---
		{"python --version", `python --version`},
	}

	for _, c := range cases {
		rr, errMsg := probe(c.cmd)
		verdict := "OK"
		switch {
		case strings.Contains(errMsg, "PowerShell, not bash") || strings.Contains(errMsg, "filesystem."):
			verdict = "HINT" // steered with actionable guidance — handled by design
		case errMsg != "" || rr.ExitCode != 0:
			verdict = "FAIL"
		}
		t.Logf("[%-5s] %-26s exit=%-3d out=%q err=%q toolErr=%q",
			verdict, c.name, rr.ExitCode, clip(rr.Stdout), clip(rr.Stderr), clip(errMsg))
	}
}
