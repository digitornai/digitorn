package background_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/background"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

func TestObservability_SettleWaiterSuppressesAsyncNotification(t *testing.T) {
	release := make(chan struct{})
	m := background.New()
	m.AttachDispatcher(blockingDispatcher(release))

	id, err := launch(m, "s", "bash.run", nil)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(release)
	}()
	st, err := m.Wait(context.Background(), "s", id, 0)
	if err != nil || st.State != "completed" {
		t.Fatalf("settle wait: err=%v state=%q", err, st.State)
	}
	if n := m.DrainNotificationsPeek("s"); len(n) != 0 {
		t.Fatalf("a settle waiter must suppress the async notification, got %d", len(n))
	}
}

func TestObservability_LiveLogWhileRunning(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	wrote := make(chan struct{})
	disp := &simpleDispatcher{body: func(ctx context.Context, _ runtime.ToolInvocation) (string, error) {
		if sink := tool.LiveSinkFromContext(ctx); sink != nil {
			_, _ = sink.Write([]byte("Listening on :3000\n"))
		}
		close(wrote)
		select {
		case <-release:
			return "final", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}}
	m := background.New()
	m.AttachDispatcher(disp)

	id, err := launch(m, "s", "bash.run", nil)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	<-wrote
	st, _ := m.Status(context.Background(), "s", id)
	if st.State != "running" {
		t.Fatalf("task should still be running, got %q", st.State)
	}
	if !strings.Contains(st.Log, "Listening on :3000") {
		t.Fatalf("Status must surface the LIVE log of a running task, got Log=%q", st.Log)
	}
}

type erroringDispatcher struct {
	errMsg string
	output string
}

func (d erroringDispatcher) Dispatch(_ context.Context, _ runtime.ToolInvocation) runtime.ToolOutcome {
	return runtime.ToolOutcome{
		Status: "errored",
		Error:  d.errMsg,
		Parts:  []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: d.output}},
	}
}

func TestObservability_FinishedTaskOutputIsConsultable(t *testing.T) {
	m := background.New()
	m.AttachDispatcher(instantDispatcher("Server build complete\nListening on :3000"))

	id, err := launch(m, "s", "bash.run", map[string]any{"command": "npm run build"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	waitFor(t, func() bool {
		st, _ := m.Status(context.Background(), "s", id)
		return st.State == "completed"
	}, "completion")

	st, _ := m.Status(context.Background(), "s", id)
	out, _ := st.Result.(string)
	if !strings.Contains(out, "Listening on :3000") {
		t.Fatalf("agent cannot consult the finished task's output: result=%q", st.Result)
	}
}

func TestObservability_AgentKnowsRunningVsStopped(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	m := background.New()
	m.AttachDispatcher(blockingDispatcher(release))

	id, err := launch(m, "s", "bash.run", map[string]any{"command": "node server.js"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	waitFor(t, func() bool {
		st, _ := m.Status(context.Background(), "s", id)
		return st.State == "running"
	}, "running state visible to agent")

	if st, _ := m.Status(context.Background(), "s", id); st.Result != nil {
		t.Fatalf("running task should not yet expose a result (output is captured on exit), got %v", st.Result)
	}

	if err := m.Cancel(context.Background(), "s", id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitFor(t, func() bool {
		st, _ := m.Status(context.Background(), "s", id)
		return st.State == "cancelled"
	}, "agent observes the server stopped")
}

func TestObservability_FailureNotificationCarriesRealOutput(t *testing.T) {
	m := background.New()
	m.AttachDispatcher(erroringDispatcher{
		errMsg: "exit code 1",
		output: `{"stdout":"","stderr":"Error: listen EADDRINUSE: address already in use :::3000","exit_code":1}`,
	})

	if _, err := launch(m, "s", "bash.run", map[string]any{"command": "node server.js"}); err != nil {
		t.Fatalf("launch: %v", err)
	}
	waitFor(t, func() bool { return len(m.DrainNotificationsPeek("s")) > 0 }, "failed notification queued")

	msg := m.DrainNotifications("s")[0].Message()
	if !strings.HasPrefix(msg, "[BACKGROUND TASK FAILED]") {
		t.Errorf("wrong prefix: %q", msg)
	}
	if !strings.Contains(msg, "EADDRINUSE") {
		t.Fatalf("failure notification dropped the real output — agent stays blind to the WHY:\n%s", msg)
	}
}

func TestObservability_WaitBlocksUntilServerStops(t *testing.T) {
	release := make(chan struct{})
	m := background.New()
	m.AttachDispatcher(blockingDispatcher(release))

	id, _ := launch(m, "s", "bash.run", map[string]any{"command": "node server.js"})

	st, err := m.Wait(context.Background(), "s", id, 0.05)
	if err == nil || st.State != "running" {
		t.Fatalf("wait on a live server: err=%v state=%q (want timeout + running)", err, st.State)
	}

	close(release)
	st, err = m.Wait(context.Background(), "s", id, 1)
	if err != nil || st.State != "completed" {
		t.Fatalf("wait after exit: err=%v state=%q (want completed)", err, st.State)
	}
}
