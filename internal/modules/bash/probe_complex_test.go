//go:build windows

package bash

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

// TestProbe_ComplexCommands drives a battery of realistic, complex commands
// through the FULL module path (run -> psChain -> anchorCmd -> frame -> shell)
// and logs the captured stdout/stderr/exit for each, so we can see exactly what
// works and what feedback the agent receives on failure.
func TestProbe_ComplexCommands(t *testing.T) {
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
		if len(s) > 200 {
			s = s[:200] + "…"
		}
		return s
	}

	cases := []struct{ name, cmd string }{
		{"native version", "node --version"},
		{"multi-statement", "$x=5; $y=10; Write-Output ($x+$y)"},
		{"&& chaining (translated)", `cd $env:TEMP && node -e "console.log('chained-ok')"`},
		{"pipe", "Write-Output abc def ghi | Measure-Object"},
		{"json output", `node -e "console.log(JSON.stringify({ok:true,nums:[1,2,3]}))"`},
		{"FAILURE with stderr", `node -e "console.error('BOOM_custom_error'); process.exit(1)"`},
		{"command not found", "nonexistent_cmd_xyz_123 --flag"},
		{"native exit code", `cmd /c "exit 7"`},
		{"git version", "git --version"},
		{"bash-ism on PowerShell (expected fail)", "grep foo somefile.txt"},
	}

	for _, c := range cases {
		rr, errMsg := probe(c.cmd)
		t.Logf("[%-38s] exit=%-4d ok=%-5v\n        stdout=%q\n        stderr=%q\n        toolErr=%q",
			c.name, rr.ExitCode, rr.ExitCode == 0, clip(rr.Stdout), clip(rr.Stderr), clip(errMsg))
	}

	// Hard guarantees (not just logging): a failing command MUST give the agent
	// the real reason + a non-zero exit.
	rr, _ := probe(`node -e "console.error('NEEDLE_42'); process.exit(1)"`)
	if rr.ExitCode == 0 {
		t.Fatalf("failing command reported exit 0")
	}
	if !strings.Contains(rr.Stdout+rr.Stderr, "NEEDLE_42") {
		t.Fatalf("agent did NOT receive the error text: stdout=%q stderr=%q", rr.Stdout, rr.Stderr)
	}
}
