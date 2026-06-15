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
// CANCEL PROOFS — extends the existing bash cancel test to cover the gaps:
//  - PowerShell on Windows (the default backend the daemon uses there).
//  - Cancel WHILE the task is streaming output: the live log must hold the
//    lines emitted before the kill, and the agent's status read must see them.
// =============================================================================

// TestPowerShellBackgroundCancel_KillsProcessAndWakesAgent is the Windows-
// PowerShell counterpart of TestBashBackgroundCancel: launches a
// `Start-Sleep 30` background task and proves Manager.Cancel kills the whole
// PowerShell process tree promptly, the task state reaches cancelled, and the
// agent is woken.
func TestPowerShellBackgroundCancel_KillsProcessAndWakesAgent(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell test is Windows-only")
	}
	m := bash.New()
	// No `shell:` config → Windows default = powershell, as the daemon ships.
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

	mgr := background.New()
	mgr.AttachDispatcher(bashDispatcher{m: m})
	w := &recordingWaker{}
	mgr.AttachWaker(w)

	const sid = "s1"
	id, err := mgr.Launch(context.Background(), meta.LaunchRequest{
		SessionID: sid, AppID: "app", UserID: "u", AgentID: "main",
		Tool: "bash.run", Args: map[string]any{"command": "Start-Sleep 30"},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	waitFor(t, func() bool {
		st, _ := mgr.Status(context.Background(), sid, id)
		return st.State == "running"
	}, "task running")
	time.Sleep(500 * time.Millisecond)

	start := time.Now()
	if err := mgr.Cancel(context.Background(), sid, id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	deadline := time.Now().Add(8 * time.Second)
	var state string
	for time.Now().Before(deadline) {
		st, _ := mgr.Status(context.Background(), sid, id)
		if state = st.State; state == "cancelled" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if state != "cancelled" {
		t.Fatalf("task did not reach cancelled state: %q", state)
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Fatalf("PowerShell cancel was not prompt: %v", elapsed)
	}

	waitFor(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.calls) > 0
	}, "agent woken on cancel")

	found := false
	for _, n := range mgr.DrainNotifications(sid) {
		if n.Status == "cancelled" && strings.Contains(n.Message(), "[BACKGROUND TASK CANCELLED]") {
			found = true
		}
	}
	if !found {
		t.Fatal("no [BACKGROUND TASK CANCELLED] notification queued for the agent")
	}
}

// TestBackgroundCancel_WithLiveLog_PreservesEmittedLines runs a streaming task,
// cancels it mid-stream, and asserts that the live log still carries the
// lines emitted before the kill. The agent reading Status after cancel must
// see WHAT got done before the cancel — silence on cancel would tell the agent
// nothing about how far the task got. Works on both bash and powershell (the
// default for the host); skips when neither is available.
func TestBackgroundCancel_WithLiveLog_PreservesEmittedLines(t *testing.T) {
	m := bash.New()
	if err := m.Init(context.Background(), map[string]any{"workdir": t.TempDir()}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !m.HasShell() {
		t.Skip("no shell available")
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(context.Background())

	// Choose a streaming command the active shell can run. The test exercises
	// the CANCEL path, not the shell — both forms emit `PROOF_LINE_<i>` so the
	// assertion is the same.
	var cmd string
	switch m.Kind() {
	case "powershell", "pwsh":
		cmd = `1..50 | ForEach-Object { Write-Output "PROOF_LINE_$_"; Start-Sleep -Milliseconds 200 }`
	default:
		cmd = `for i in $(seq 1 50); do printf 'PROOF_LINE_%d\n' "$i"; sleep 0.2; done`
	}

	mgr := background.New()
	mgr.AttachDispatcher(bashDispatcher{m: m})

	const sid = "s1"
	id, err := mgr.Launch(context.Background(), meta.LaunchRequest{
		SessionID: sid, AppID: "app", UserID: "u", AgentID: "main",
		Tool: "bash.run", Args: map[string]any{"command": cmd, "timeout_seconds": 60},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// Wait until at least 3 lines have streamed, then cancel mid-stream.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := mgr.Status(context.Background(), sid, id)
		if strings.Count(st.Log, "PROOF_LINE_") >= 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := mgr.Cancel(context.Background(), sid, id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// Drain to final state and inspect the log.
	deadline = time.Now().Add(8 * time.Second)
	var final meta.BackgroundStatus
	for time.Now().Before(deadline) {
		st, _ := mgr.Status(context.Background(), sid, id)
		final = st
		if st.State == "cancelled" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if final.State != "cancelled" {
		t.Fatalf("task did not reach cancelled, got %q", final.State)
	}
	if !strings.Contains(final.Log, "PROOF_LINE_1") {
		t.Fatalf("live log lost the lines emitted before cancel:\n%s", final.Log)
	}
	// The task ran for ~600 ms before cancel (3 lines × 200 ms), so it cannot
	// have reached line 50.
	if strings.Contains(final.Log, "PROOF_LINE_50") {
		t.Errorf("task somehow finished — cancel did not stop it in time:\n%s", final.Log)
	}
	t.Logf("OK — cancel preserved %d streamed line(s) for the agent:\n%s",
		strings.Count(final.Log, "PROOF_LINE_"), final.Log)
}
