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

func TestPowerShellBackgroundCancel_KillsProcessAndWakesAgent(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell test is Windows-only")
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
	if strings.Contains(final.Log, "PROOF_LINE_50") {
		t.Errorf("task somehow finished — cancel did not stop it in time:\n%s", final.Log)
	}
	t.Logf("OK — cancel preserved %d streamed line(s) for the agent:\n%s",
		strings.Count(final.Log, "PROOF_LINE_"), final.Log)
}
