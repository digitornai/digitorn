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

// TestObservability_SettleWaiterSuppressesAsyncNotification : when a task
// finishes WHILE a Wait (the settle window) is consuming it, no async
// completion notification is enqueued — the agent gets the result once
// (synchronously), not a confusing duplicate "[BACKGROUND TASK …]" too.
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
		close(release) // let the task finish WHILE Wait is in flight
	}()
	st, err := m.Wait(context.Background(), "s", id, 0) // no timeout → returns when done
	if err != nil || st.State != "completed" {
		t.Fatalf("settle wait: err=%v state=%q", err, st.State)
	}
	if n := m.DrainNotificationsPeek("s"); len(n) != 0 {
		t.Fatalf("a settle waiter must suppress the async notification, got %d", len(n))
	}
}

// TestObservability_LiveLogWhileRunning : a still-running task streams its
// output into the live sink, and a Status check surfaces that tail BEFORE the
// task finishes — so the agent can watch a server's startup / spot an error.
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
	<-wrote // the task has streamed to the live sink and is still running
	st, _ := m.Status(context.Background(), "s", id)
	if st.State != "running" {
		t.Fatalf("task should still be running, got %q", st.State)
	}
	if !strings.Contains(st.Log, "Listening on :3000") {
		t.Fatalf("Status must surface the LIVE log of a running task, got Log=%q", st.Log)
	}
}

// erroringDispatcher returns an errored outcome that ALSO carries output Parts
// (stdout/stderr), mirroring what busadapter does for a failed bash.run — the
// command failed but its output is the WHY.
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

// TestObservability_FinishedTaskOutputIsConsultable : after a backgrounded
// command finishes, the agent can read its captured output via background_run
// status (the Result field). Answers "peut-il consulter la sortie ?".
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

// TestObservability_AgentKnowsRunningVsStopped : a long-running server reports
// state "running" while alive, and flips to "cancelled" once stopped — the
// agent can tell a live server from a dead one. Answers "savoir si un serveur
// tourne encore / s'est arrêté ?".
func TestObservability_AgentKnowsRunningVsStopped(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	m := background.New()
	m.AttachDispatcher(blockingDispatcher(release)) // models a server that never returns on its own

	id, err := launch(m, "s", "bash.run", map[string]any{"command": "node server.js"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// While the "server" runs, the agent polling status sees "running".
	waitFor(t, func() bool {
		st, _ := m.Status(context.Background(), "s", id)
		return st.State == "running"
	}, "running state visible to agent")

	// A still-running task has no captured Result yet (the dispatch hasn't
	// returned) — this is the honest limit: live stdout is not streamed.
	if st, _ := m.Status(context.Background(), "s", id); st.Result != nil {
		t.Fatalf("running task should not yet expose a result (output is captured on exit), got %v", st.Result)
	}

	// The agent stops the server.
	if err := m.Cancel(context.Background(), "s", id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitFor(t, func() bool {
		st, _ := m.Status(context.Background(), "s", id)
		return st.State == "cancelled"
	}, "agent observes the server stopped")
}

// TestObservability_FailureNotificationCarriesRealOutput : the auto-notification
// injected on the next turn for a FAILED background task must include the
// captured output (e.g. EADDRINUSE), not just "it failed". This pins the
// runTask → CompletionNotification.Output wiring end to end (a struct-only test
// would miss a producer that never sets the field). Answers "avoir des retours
// sur les bugs ?".
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

// TestObservability_WaitBlocksUntilServerStops : an agent can block on a task
// with a bounded timeout (background_run wait) and gets a timeout (still
// running) vs a terminal snapshot — a third way to poll liveness.
func TestObservability_WaitBlocksUntilServerStops(t *testing.T) {
	release := make(chan struct{})
	m := background.New()
	m.AttachDispatcher(blockingDispatcher(release))

	id, _ := launch(m, "s", "bash.run", map[string]any{"command": "node server.js"})

	// Short wait while it's alive → timeout, snapshot still "running".
	st, err := m.Wait(context.Background(), "s", id, 0.05)
	if err == nil || st.State != "running" {
		t.Fatalf("wait on a live server: err=%v state=%q (want timeout + running)", err, st.State)
	}

	// Server exits → wait returns the terminal snapshot.
	close(release)
	st, err = m.Wait(context.Background(), "s", id, 1)
	if err != nil || st.State != "completed" {
		t.Fatalf("wait after exit: err=%v state=%q (want completed)", err, st.State)
	}
}
