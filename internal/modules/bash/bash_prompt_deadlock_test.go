package bash

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
)

// TestS8_ReadPromptDoesNotDeadlockTheTurn proves the audit-flagged S8 case:
// a command that calls `read -p` for interactive input must NOT freeze the
// agent loop until the configured timeout. The frame redirects stdin from
// /dev/null, so `read` sees EOF immediately and returns. If this test ever
// goes red, S8 is real and we owe the agent an explicit `</dev/null` on each
// inner read or a `timeout` wrapper.
func TestS8_ReadPromptDoesNotDeadlockTheTurn(t *testing.T) {
	kind, path, err := detectShell("bash")
	if err != nil || kind != "bash" {
		t.Skip("bash not available on this host")
	}
	_ = path

	m := New()
	m.cfg.Workdir = t.TempDir()
	m.cfg.TimeoutSecs = 30
	// Do NOT pin: we want to test the DEFAULT path the daemon picks at runtime.
	if err := m.Init(context.Background(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Logf("DEFAULT shell: kind=%s path=%s useGoShell=%v useMvdan=%v", m.kind, m.path, m.useGoShell, m.useMvdanSh)
	defer func() { _ = m.Stop(context.Background()) }()

	ctx := tool.WithIdentity(context.Background(), tool.Identity{AppID: "app", SessionID: "s8"})
	raw, _ := json.Marshal(runParams{
		Command:        `read -p "enter: " x; echo "X=[$x]"`,
		TimeoutSeconds: 10, // headroom for cold-start PowerShell under -race
	})

	start := time.Now()
	res, err := m.run(ctx, raw)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// A real deadlock would take at least the full timeout (10s). Anything
	// under 8s means the read attempt failed fast (the desired outcome) even
	// after PowerShell cold-start under -race.
	if elapsed > 8*time.Second {
		t.Fatalf("S8 reproduced: read -p deadlocked, took %v (timeout was 10s)", elapsed)
	}
	rr, _ := res.Data.(runResult)
	if rr.TimedOut {
		t.Fatalf("S8 reproduced: read -p timed out after %v — agent loop would have stalled", elapsed)
	}
	if !strings.Contains(rr.Stdout, "X=") {
		t.Logf("info: stdout=%q stderr=%q exit=%d", rr.Stdout, rr.Stderr, rr.ExitCode)
	}
	t.Logf("OK — read -p returned in %v (no deadlock); stdout=%q", elapsed, strings.TrimSpace(rr.Stdout))
}
