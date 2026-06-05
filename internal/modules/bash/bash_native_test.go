package bash

import (
	"strings"
	"testing"
)

// TestBashNative_ConformancePasses : with the one-shot real-bash executor, the
// bash-muscle-memory that used to FAIL (or only get a hint) on the PowerShell
// shell now runs NATIVELY — control-flow, conditionals, export, command
// substitution and /dev/null all just work, with no translation layer. This is
// the headline of the one-shot pivot. Skipped on a host without bash.
func TestBashNative_ConformancePasses(t *testing.T) {
	m := testModule(t) // shell:"bash" ; t.Skip if no bash on host
	cases := []struct{ name, cmd, want string }{
		{"for-loop", `for i in 1 2 3; do echo "n$i"; done`, "n2"},
		{"if-test", `if [ -d . ]; then echo ISDIR; fi`, "ISDIR"},
		{"while-loop", `i=0; while [ $i -lt 2 ]; do echo "w$i"; i=$((i+1)); done`, "w1"},
		{"export-inline", `export GREET=bonjour && echo "[$GREET]"`, "[bonjour]"},
		{"dev-null", `echo keep 2>/dev/null`, "keep"},
		{"test-and-or", `[ -e definitely_absent ] && echo yes || echo no`, "no"},
		{"cmd-subst", `echo "v=$(echo 42)"`, "v=42"},
		{"arith", `echo $((6 * 7))`, "42"},
		{"chain", `true && echo CHAINED`, "CHAINED"},
	}
	for _, c := range cases {
		rr := invoke(t, m, "native", c.cmd)
		if rr.ExitCode != 0 {
			t.Errorf("[%s] %q → exit=%d stderr=%q (real bash must run this natively)", c.name, c.cmd, rr.ExitCode, rr.Stderr)
			continue
		}
		if !strings.Contains(rr.Stdout, c.want) {
			t.Errorf("[%s] %q → want %q in stdout, got %q", c.name, c.cmd, c.want, rr.Stdout)
		}
	}
}
