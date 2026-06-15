package background_test

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/modules/bash"
	"github.com/mbathepaul/digitorn/internal/runtime/background"
	"github.com/mbathepaul/digitorn/internal/runtime/context/meta"
)

// =============================================================================
// LIVE-LOG PROOFS — these tests run REAL shell commands (PowerShell + bash)
// through the production pipeline: bash module → BackgroundManager → live sink
// → Status. They prove the agent sees the streamed output WHILE the task is
// still running, not just at completion.
// =============================================================================

// runShellStreamingTask is the shared driver: it launches a background command
// that emits N lines with a short delay between each, polls Status until the
// first chunk of live output lands, then waits for completion. Returns the
// mid-run status (proof of live streaming) and the final status (proof the
// log survived to the end).
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

	// Poll Status until the running task has at least `wantLines` of streamed
	// output. Deadline is generous (10s) — proves we don't have to WAIT for
	// completion to see progress.
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
			// Task finished before we got the mid-run snapshot we wanted.
			t.Logf("task finished before mid-run snapshot — state=%q log=%q", st.State, st.Log)
			mid = st
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if mid.TaskID == "" {
		t.Fatalf("never saw %d live lines within 10s", wantLines)
	}

	// Drain to completion.
	final, err = mgr.Wait(context.Background(), sid, id, 15)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	return mid, final
}

// TestBashBackground_LiveLog_PowerShell proves the live tail works on the
// Windows-default backend: a real PowerShell task streams 10 lines with
// 100 ms cadence; Status reads in flight surface the lines that have been
// emitted SO FAR — not zero, not all-or-nothing.
func TestBashBackground_LiveLog_PowerShell(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell live-log test is Windows-only")
	}
	m := bash.New()
	// No `shell` config: on Windows the module's default detection picks
	// powershell → pwsh → mvdan/sh fallback. Explicitly passing shell:
	// "powershell" would route through the Linux/macOS branch in Init and
	// force the mvdan fallback (kind!=bash/sh ⇒ useMvdanSh=true), which we
	// proved by the previous "ForEach-Object: not found in $PATH" error.
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

	// 1..10 emits to stdout with a 100 ms gap → ~1 s total, plenty of mid-run
	// snapshots. Write-Host bypasses the pipeline so output is unbuffered.
	cmd := `1..10 | ForEach-Object { Write-Output "live-line-$_"; Start-Sleep -Milliseconds 100 }`
	mid, final := runShellStreamingTask(t, m, cmd, 3)

	if !strings.Contains(mid.Log, "live-line-1") {
		t.Errorf("mid-run log did not carry the first streamed line:\n%s", mid.Log)
	}
	t.Logf("mid-run snapshot (state=%s):\n%s", mid.State, mid.Log)

	if final.State != "completed" {
		t.Fatalf("final state=%q want completed (error=%q)", final.State, final.Error)
	}
	// At completion the bounded rolling log should still carry the LAST lines.
	if !strings.Contains(final.Log, "live-line-10") {
		t.Errorf("post-completion log missing the last line:\n%s", final.Log)
	}
	t.Logf("final snapshot (state=%s):\n%s", final.State, final.Log)
}

// TestBashBackground_LiveLog_Bash is the same proof against a real bash
// process (Git-Bash on Windows, native bash on Linux/macOS). Skips when no
// bash is on PATH so it doesn't break CI on stripped Windows images.
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

	// Bash: emit 10 lines with 100 ms gap. printf so the line-ending is
	// well-defined across shells.
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
