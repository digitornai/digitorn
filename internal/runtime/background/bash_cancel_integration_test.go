package background_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/modules/bash"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/background"
	"github.com/digitornai/digitorn/internal/runtime/context/meta"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// bashDispatcher routes a background ToolInvocation into the real bash module,
// exactly as the production bus adapter does — including threading the ctx
// (already marked WithBackground by the manager) so bash runs the command in an
// independent, separately-cancellable process.
type bashDispatcher struct{ m *bash.Module }

func (d bashDispatcher) Dispatch(ctx context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	ctx = tool.WithIdentity(ctx, tool.Identity{AppID: call.AppID, SessionID: call.SessionID, UserID: call.UserID, AgentID: call.AgentID})
	action := strings.TrimPrefix(call.Name, "bash.")
	args, _ := json.Marshal(call.Args)
	res, err := d.m.Invoke(ctx, action, args)
	out := runtime.ToolOutcome{Status: "completed"}
	if err != nil || !res.Success {
		out.Status = "errored"
		out.Error = res.Error
	}
	b, _ := json.Marshal(res.Data)
	out.Parts = []sessionstore.MessagePart{{Text: string(b)}}
	return out
}

// TestBashBackgroundCancel_KillsProcessAndWakesAgent proves the full client
// cancellation path with a REAL bash process: a background `sleep` task is
// cancelled the same way the REST endpoint does (Manager.Cancel), the process
// is killed promptly, the task ends in state "cancelled", the agent is woken
// instantly (WakeSession), and a [BACKGROUND TASK CANCELLED] notification is
// queued for its next turn.
func TestBashBackgroundCancel_KillsProcessAndWakesAgent(t *testing.T) {
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

	mgr := background.New()
	mgr.AttachDispatcher(bashDispatcher{m: m})
	w := &recordingWaker{}
	mgr.AttachWaker(w)

	const sid = "s1"
	id, err := mgr.Launch(context.Background(), meta.LaunchRequest{
		SessionID: sid, AppID: "app", UserID: "u", AgentID: "main",
		Tool: "bash.run", Args: map[string]any{"command": "sleep 30"},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	waitFor(t, func() bool {
		st, _ := mgr.Status(context.Background(), sid, id)
		return st.State == "running"
	}, "task running")
	time.Sleep(500 * time.Millisecond) // let the sleep process actually spawn

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
		t.Fatalf("cancellation was not prompt: %v (process tree not killed)", elapsed)
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
