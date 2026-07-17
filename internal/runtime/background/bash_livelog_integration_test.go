package background_test

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/modules/bash"
	"github.com/digitornai/digitorn/internal/runtime/background"
	"github.com/digitornai/digitorn/internal/runtime/context/meta"
)

func runShellStreamingTask(
	t *testing.T,
	m *bash.Module,
	command string,
	wantLines int,
) (mid, final meta.BackgroundStatus) {
	t.Helper()
	mgr := background.New()
	mgr.AttachDispatcher(bashDispatcher{m: m})

	const sid = "s1"
	id, err := mgr.Launch(context.Background(), meta.LaunchRequest{
		SessionID: sid, AppID: "app", UserID: "u", AgentID: "main",
		Tool: "bash.run", Args: map[string]any{"command": command, "timeout_seconds": 30},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := mgr.Status(context.Background(), sid, id)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if strings.Count(st.Log, "live-line-") >= wantLines {
			mid = st
			break
		}
		if st.State == "completed" || st.State == "errored" {
			t.Logf("task finished before mid-run snapshot — state=%q log=%q", st.State, st.Log)
			mid = st
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if mid.TaskID == "" {
		t.Fatalf("never saw %d live lines within 10s", wantLines)
	}

	final, err = mgr.Wait(context.Background(), sid, id, 15)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	return mid, final
}

func TestBashBackground_LiveLog_PowerShell(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell live-log test is Windows-only")
	}
	m := bash.New()
	if err := m.Init(context.Background(), map[string]any{"workdir": t.TempDir()}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !m.HasShell() {
		t.Skip("no powershell available")
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(context.Background())

	cmd := `1..10 | ForEach-Object { Write-Output "live-line-$_"; Start-Sleep -Milliseconds 100 }`
	mid, final := runShellStreamingTask(t, m, cmd, 3)

	if !strings.Contains(mid.Log, "live-line-1") {
		t.Errorf("mid-run log did not carry the first streamed line:\n%s", mid.Log)
	}
	t.Logf("mid-run snapshot (state=%s):\n%s", mid.State, mid.Log)

	if final.State != "completed" {
		t.Fatalf("final state=%q want completed (error=%q)", final.State, final.Error)
	}
	if !strings.Contains(final.Log, "live-line-10") {
		t.Errorf("post-completion log missing the last line:\n%s", final.Log)
	}
	t.Logf("final snapshot (state=%s):\n%s", final.State, final.Log)
}

func TestBashBackground_LiveLog_Bash(t *testing.T) {
	m := bash.New()
	if err := m.Init(context.Background(), map[string]any{"workdir": t.TempDir(), "shell": "bash"}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !m.HasShell() {
		t.Skip("no bash available")
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(context.Background())

	cmd := `for i in 1 2 3 4 5 6 7 8 9 10; do printf 'live-line-%d\n' "$i"; sleep 0.1; done`
	mid, final := runShellStreamingTask(t, m, cmd, 3)

	if !strings.Contains(mid.Log, "live-line-1") {
		t.Errorf("mid-run log did not carry the first streamed line:\n%s", mid.Log)
	}
	t.Logf("mid-run snapshot (state=%s):\n%s", mid.State, mid.Log)

	if final.State != "completed" {
		t.Fatalf("final state=%q want completed (error=%q)", final.State, final.Error)
	}
	if !strings.Contains(final.Log, "live-line-10") {
		t.Errorf("post-completion log missing the last line:\n%s", final.Log)
	}
	t.Logf("final snapshot (state=%s):\n%s", final.State, final.Log)
}
