package bash

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

func TestDosHint(t *testing.T) {
	flagged := []string{
		"dir /B /S",
		"cd my-react-app && dir /B /S",
		"copy a b", "xcopy /E src dst", "del foo.txt", "erase foo",
		"move a b", "ren a b", "rename a b", "cls", "findstr foo *.txt",
		"echo hi && cls", "ipconfig && dir", "mkdir x && del y",
	}
	for _, c := range flagged {
		if dosHint(c) == "" {
			t.Errorf("expected a DOS hint for %q, got none", c)
		}
	}
	allowed := []string{
		"ls -R", "find . -type f", "cat package.json", "cp a b", "rm -rf foo",
		"mv a b", "grep -r foo .", "cd /usr/bin && ls", "ls /etc", "ls /bin",
		"git status", "node -v", "npm run build", `echo "dir /B /S"`,
		"ls -la", "mkdir -p a/b/c", "cd src && ls", "pwd",
	}
	for _, c := range allowed {
		if h := dosHint(c); h != "" {
			t.Errorf("did NOT expect a DOS hint for %q, got: %s", c, h)
		}
	}
}

func TestRun_DosCommandRejected(t *testing.T) {
	m := testModule(t)
	if m.useGoShell || m.kind != "powershell" {
		ctx := tool.WithIdentity(context.Background(), tool.Identity{AppID: "app", SessionID: "s"})
		start := time.Now()
		raw, _ := json.Marshal(runParams{Command: "cd my-react-app && dir /B /S"})
		res, _ := m.run(ctx, raw)
		if time.Since(start) > 2*time.Second {
			t.Fatalf("DOS command was not rejected fast")
		}
		if res.Error == "" || !strings.Contains(strings.ToLower(res.Error), "bash shell") {
			t.Fatalf("expected a bash-shell guidance error, got data=%+v error=%q", res.Data, res.Error)
		}
	}
}
