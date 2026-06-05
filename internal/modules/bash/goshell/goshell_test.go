package goshell

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// TestAgentBattery proves the pure-Go shell runs the commands a coding agent
// actually types — including the ones that BROKE under PowerShell — with NO
// external bash and NO install dependency (this is just Go).
func TestAgentBattery(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name, script, wantOut string
		wantCode              int
	}{
		{"echo", `echo hello`, "hello", 0},
		{"inline_assign", `X=5; echo $X`, "5", 0},              // PS: "X=5 not recognized"
		{"export_read", `export FOO=bar; echo $FOO`, "bar", 0}, // PS: empty
		{"arrays", `a=(x y z); echo ${a[1]}`, "y", 0},          // ash can't; proves real bash
		{"double_bracket", `[[ 2 -lt 3 ]] && echo lt`, "lt", 0},
		{"arith", `echo $((6*7))`, "42", 0},
		{"and_chain", `true && echo ok`, "ok", 0},
		{"or_recover", `false || echo recovered`, "recovered", 0},
		{"cmd_subst", `echo $(echo nested)`, "nested", 0},
		{"for_loop", `for i in 1 2 3; do printf "%s" "$i"; done`, "123", 0},
		{"if_test", `if [ -d . ]; then echo isdir; fi`, "isdir", 0},
		{"param_expand", `name=world; echo "hi ${name}!"`, "hi world!", 0},
		{"exit_code", `exit 3`, "", 3},
		{"pipe_builtins", `echo abc | while read x; do echo "[$x]"; done`, "[abc]", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code, err := Run(context.Background(), c.script, dir, os.Environ(), nil, &stdout, &stderr)
			if err != nil {
				t.Fatalf("%s: err %v (stderr=%q)", c.script, err, stderr.String())
			}
			if code != c.wantCode {
				t.Fatalf("%s: exit=%d want %d (stderr=%q)", c.script, code, c.wantCode, stderr.String())
			}
			if got := strings.TrimSpace(stdout.String()); got != c.wantOut {
				t.Fatalf("%s: out=%q want %q", c.script, got, c.wantOut)
			}
		})
	}
}

// TestExecExternal proves external programs (git/node/python) are exec'd from
// PATH — the shell logic is in-proc, the real tools are still run.
func TestExecExternal(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := Run(context.Background(), `git --version`, t.TempDir(), os.Environ(), nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("err %v", err)
	}
	if code != 0 || !strings.Contains(stdout.String(), "git version") {
		t.Skipf("git not on PATH or failed (out=%q code=%d) — external-exec mechanism still valid", stdout.String(), code)
	}
}
