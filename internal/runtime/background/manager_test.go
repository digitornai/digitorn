package background_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/background"
	"github.com/digitornai/digitorn/internal/runtime/context/meta"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

func launch(m *background.Manager, sid, tool string, args map[string]any) (string, error) {
	return m.Launch(context.Background(), meta.LaunchRequest{SessionID: sid, Tool: tool, Args: args})
}


type simpleDispatcher struct {
	body       func(ctx context.Context, c runtime.ToolInvocation) (string, error)
	dispatched atomic.Int64
}

func (s *simpleDispatcher) Dispatch(ctx context.Context, c runtime.ToolInvocation) runtime.ToolOutcome {
	s.dispatched.Add(1)
	out, err := s.body(ctx, c)
	if err != nil {
		return runtime.ToolOutcome{Status: "errored", Error: err.Error()}
	}
	return runtime.ToolOutcome{
		Status: "completed",
		Parts: []sessionstore.MessagePart{
			{Type: sessionstore.PartTypeText, Text: out},
		},
	}
}

func instantDispatcher(text string) *simpleDispatcher {
	return &simpleDispatcher{
		body: func(_ context.Context, _ runtime.ToolInvocation) (string, error) {
			return text, nil
		},
	}
}

func blockingDispatcher(release <-chan struct{}) *simpleDispatcher {
	return &simpleDispatcher{
		body: func(ctx context.Context, _ runtime.ToolInvocation) (string, error) {
			select {
			case <-release:
				return "released", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	}
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", msg)
}

func TestManager_LaunchWithoutDispatcher(t *testing.T) {
	m := background.New()
	_, err := launch(m, "s", "x.y", nil)
	if err == nil {
		t.Error("expected error when dispatcher not attached")
	}
}

func TestManager_LaunchAndStatus(t *testing.T) {
	m := background.New()
	disp := instantDispatcher("hello")
	m.AttachDispatcher(disp)

	id, err := launch(m, "s", "tool.x", map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if id == "" {
		t.Fatal("Launch returned empty id")
	}

	waitFor(t, func() bool {
		st, _ := m.Status(context.Background(), "s", id)
		return st.State == "completed"
	}, "task completion")

	st, err := m.Status(context.Background(), "s", id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != "completed" {
		t.Errorf("state = %q", st.State)
	}
	if st.Name != "tool.x" {
		t.Errorf("name = %q", st.Name)
	}
	if st.Result != "hello" {
		t.Errorf("result = %v", st.Result)
	}
	if disp.dispatched.Load() != 1 {
		t.Errorf("dispatched = %d", disp.dispatched.Load())
	}
}

func TestManager_TaskError(t *testing.T) {
	disp := &simpleDispatcher{
		body: func(_ context.Context, _ runtime.ToolInvocation) (string, error) {
			return "", errors.New("boom")
		},
	}
	m := background.New()
	m.AttachDispatcher(disp)

	id, _ := launch(m, "s", "x", nil)
	waitFor(t, func() bool {
		st, _ := m.Status(context.Background(), "s", id)
		return st.State == "errored"
	}, "errored state")

	st, _ := m.Status(context.Background(), "s", id)
	if !strings.Contains(st.Error, "boom") {
		t.Errorf("err = %q", st.Error)
	}
}

func TestManager_WaitReturnsCompletion(t *testing.T) {
	release := make(chan struct{})
	disp := blockingDispatcher(release)
	m := background.New()
	m.AttachDispatcher(disp)

	id, _ := launch(m, "s", "x", nil)

	go func() {
		time.Sleep(20 * time.Millisecond)
		close(release)
	}()

	st, err := m.Wait(context.Background(), "s", id, 0)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if st.State != "completed" {
		t.Errorf("state = %q", st.State)
	}
}

func TestManager_WaitTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	disp := blockingDispatcher(release)
	m := background.New()
	m.AttachDispatcher(disp)

	id, _ := launch(m, "s", "x", nil)

	st, err := m.Wait(context.Background(), "s", id, 0.05)
	if err == nil {
		t.Error("expected timeout error")
	}
	if st.State != "running" {
		t.Errorf("state on timeout = %q", st.State)
	}
}

func TestManager_Cancel(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	disp := blockingDispatcher(release)
	m := background.New()
	m.AttachDispatcher(disp)

	id, _ := launch(m, "s", "x", nil)
	time.Sleep(10 * time.Millisecond)

	if err := m.Cancel(context.Background(), "s", id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitFor(t, func() bool {
		st, _ := m.Status(context.Background(), "s", id)
		return st.State == "cancelled"
	}, "cancelled state")
}

func TestManager_CancelUnknownTask(t *testing.T) {
	m := background.New()
	m.AttachDispatcher(instantDispatcher("x"))
	err := m.Cancel(context.Background(), "s", "no-such-id")
	if err == nil {
		t.Error("cancel of unknown task should error")
	}
}

func TestManager_CancelAllForSession(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	m := background.New()
	m.AttachDispatcher(blockingDispatcher(release))

	a, _ := launch(m, "s1", "x", nil)
	b, _ := launch(m, "s1", "y", nil)
	keep, _ := launch(m, "s2", "z", nil)
	time.Sleep(10 * time.Millisecond)

	if n := m.CancelAllForSession("s1"); n != 2 {
		t.Fatalf("CancelAllForSession stopped %d tasks, want 2", n)
	}
	for _, id := range []string{a, b} {
		waitFor(t, func() bool {
			st, _ := m.Status(context.Background(), "s1", id)
			return st.State == "cancelled"
		}, "task "+id+" cancelled")
	}
	if st, _ := m.Status(context.Background(), "s2", keep); st.State != "running" {
		t.Errorf("task in another session must survive, got %q", st.State)
	}

	if n := m.CancelAllForSession("nope"); n != 0 {
		t.Errorf("CancelAllForSession on unknown session = %d, want 0", n)
	}
	m.CancelAllForSession("s2")
}

func TestManager_ListAllTasks(t *testing.T) {
	m := background.New()
	m.AttachDispatcher(instantDispatcher("ok"))

	for i := 0; i < 5; i++ {
		_, err := launch(m, "s", "tool.x", nil)
		if err != nil {
			t.Fatalf("Launch[%d]: %v", i, err)
		}
	}

	tasks, err := m.List(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 5 {
		t.Errorf("List = %d tasks, want 5", len(tasks))
	}
}

func TestManager_ReapsOldCompletedTasks(t *testing.T) {
	m := background.New()
	m.AttachDispatcher(instantDispatcher("ok"))
	m.RetainCompleted = 10 * time.Millisecond

	id, _ := launch(m, "s", "x", nil)
	waitFor(t, func() bool {
		st, _ := m.Status(context.Background(), "s", id)
		return st.State == "completed"
	}, "completion")

	time.Sleep(50 * time.Millisecond)

	tasks, _ := m.List(context.Background(), "s")
	if len(tasks) != 0 {
		t.Errorf("reap failed, got %d remaining", len(tasks))
	}
}

func TestManager_SessionIsolation(t *testing.T) {
	m := background.New()
	m.AttachDispatcher(instantDispatcher("ok"))

	id1, _ := launch(m, "session-A", "x", nil)
	id2, _ := launch(m, "session-B", "x", nil)

	if _, err := m.Status(context.Background(), "session-A", id2); err == nil {
		t.Error("session A should not see session B's task")
	}
	if _, err := m.Status(context.Background(), "session-B", id1); err == nil {
		t.Error("session B should not see session A's task")
	}
}

func TestManager_PerSessionCap(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	disp := blockingDispatcher(release)
	m := background.New()
	m.AttachDispatcher(disp)
	m.MaxTasksPerSession = 3

	for i := 0; i < 3; i++ {
		_, err := launch(m, "s", "x", nil)
		if err != nil {
			t.Fatalf("Launch[%d]: %v", i, err)
		}
	}
	_, err := launch(m, "s", "x", nil)
	if err == nil {
		t.Error("4th launch should hit cap")
	}
}

func TestManager_ConcurrentLaunches(t *testing.T) {
	m := background.New()
	m.AttachDispatcher(instantDispatcher("ok"))

	const N = 100
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			_, err := launch(m, "s", "x", map[string]any{"i": i})
			if err != nil {
				t.Errorf("[%d] Launch: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	tasks, _ := m.List(context.Background(), "s")
	if len(tasks) != N {
		t.Errorf("got %d tasks, want %d", len(tasks), N)
	}
}

func TestManager_EmitsNotificationOnCompletion(t *testing.T) {
	m := background.New()
	m.AttachDispatcher(instantDispatcher("ok"))

	_, err := launch(m, "s", "database.sql", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		return len(m.DrainNotificationsPeek("s")) > 0
	}, "notification emitted")

	notif := m.DrainNotifications("s")
	if len(notif) != 1 {
		t.Fatalf("want 1 notif, got %d", len(notif))
	}
	msg := notif[0].Message()
	if !strings.HasPrefix(msg, "[BACKGROUND TASK COMPLETED]") {
		t.Errorf("prefix wrong : %q", msg)
	}
	if !strings.Contains(msg, "tool=database.sql") {
		t.Errorf("tool name missing : %q", msg)
	}
	if !strings.Contains(msg, "task_id=") {
		t.Errorf("task_id missing : %q", msg)
	}
}

func TestManager_EmitsFailedNotification(t *testing.T) {
	disp := &simpleDispatcher{
		body: func(_ context.Context, _ runtime.ToolInvocation) (string, error) {
			return "", errors.New("oops")
		},
	}
	m := background.New()
	m.AttachDispatcher(disp)

	_, err := launch(m, "s", "x.y", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		return len(m.DrainNotificationsPeek("s")) > 0
	}, "errored notif")

	notif := m.DrainNotifications("s")
	if !strings.HasPrefix(notif[0].Message(), "[BACKGROUND TASK FAILED]") {
		t.Errorf("failed prefix wrong : %q", notif[0].Message())
	}
}

type bgCtxKey string

// TestManager_TaskInheritsLaunchCtxValuesOutlivingTurn pins two invariants that
// together fixed a backgrounded `node app.js` failing with exit 1: the task must
// (1) inherit the launch ctx's VALUES — the session workdir (PathPolicy) lives
// there, so the command runs in the session dir and not the daemon cwd — and
// (2) NOT die when the launch ctx is cancelled (the turn ending is fire-and-forget).
func TestManager_TaskInheritsLaunchCtxValuesOutlivingTurn(t *testing.T) {
	const key bgCtxKey = "workdir"
	gotVal := make(chan string, 1)
	release := make(chan struct{})
	disp := &simpleDispatcher{
		body: func(ctx context.Context, _ runtime.ToolInvocation) (string, error) {
			v, _ := ctx.Value(key).(string)
			gotVal <- v
			select {
			case <-release:
				return "done", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	}
	m := background.New()
	m.AttachDispatcher(disp)

	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), key, "/session/wd"))
	if _, err := m.Launch(ctx, meta.LaunchRequest{SessionID: "s", Tool: "bash.run"}); err != nil {
		t.Fatal(err)
	}
	cancel() // the turn ends right after launch — must NOT kill the task

	if v := <-gotVal; v != "/session/wd" {
		t.Fatalf("task lost the launch-ctx workdir value: got %q (would run in the wrong directory)", v)
	}
	close(release) // task was still alive (not cancelled with the turn) — let it finish
	waitFor(t, func() bool { return len(m.DrainNotificationsPeek("s")) > 0 }, "task settled")
	if n := m.DrainNotifications("s"); n[0].Status != "completed" {
		t.Fatalf("task did not survive the turn ending: status=%q (the launch ctx cancel killed it)", n[0].Status)
	}
}

// TestCompletionNotification_FailureCarriesOutput pins that a failed task's
// notification includes the captured output (the WHY: e.g. EADDRINUSE) so the
// agent isn't blind, while a completed task stays terse.
func TestCompletionNotification_FailureCarriesOutput(t *testing.T) {
	fail := background.CompletionNotification{
		TaskID: "t1", ToolName: "bash.run", ElapsedMs: 2900, Status: "errored",
		Output: `{"stderr":"Error: listen EADDRINUSE: address already in use :::3000"}`,
	}
	m := fail.Message()
	if !strings.Contains(m, "EADDRINUSE") {
		t.Errorf("failure notification dropped the output — agent stays blind: %q", m)
	}
	done := background.CompletionNotification{
		TaskID: "t2", ToolName: "bash.run", ElapsedMs: 100, Status: "completed",
		Output: "noisy success output",
	}
	if strings.Contains(done.Message(), "noisy success output") {
		t.Errorf("completed notification should stay terse: %q", done.Message())
	}
}

func TestManager_DrainNotificationsClears(t *testing.T) {
	m := background.New()
	m.AttachDispatcher(instantDispatcher("ok"))
	_, _ = launch(m, "s", "x", nil)
	waitFor(t, func() bool { return len(m.DrainNotificationsPeek("s")) > 0 }, "queued")
	_ = m.DrainNotifications("s")
	if got := m.DrainNotifications("s"); len(got) != 0 {
		t.Errorf("second drain returned %d entries", len(got))
	}
}

func TestManager_NotificationsAreSessionScoped(t *testing.T) {
	m := background.New()
	m.AttachDispatcher(instantDispatcher("ok"))
	_, _ = launch(m, "sessA", "x", nil)
	_, _ = launch(m, "sessB", "y", nil)
	waitFor(t, func() bool {
		return len(m.DrainNotificationsPeek("sessA")) > 0 &&
			len(m.DrainNotificationsPeek("sessB")) > 0
	}, "both queues populated")

	if got := m.DrainNotifications("sessA"); len(got) != 1 || got[0].ToolName != "x" {
		t.Errorf("sessA wrong : %+v", got)
	}
	if got := m.DrainNotifications("sessB"); len(got) != 1 || got[0].ToolName != "y" {
		t.Errorf("sessB wrong : %+v", got)
	}
}

func TestManager_WaitOnAlreadyFinishedTask(t *testing.T) {
	m := background.New()
	m.AttachDispatcher(instantDispatcher("ok"))

	id, _ := launch(m, "s", "x", nil)
	waitFor(t, func() bool {
		st, _ := m.Status(context.Background(), "s", id)
		return st.State == "completed"
	}, "completion")

	st, err := m.Wait(context.Background(), "s", id, 1)
	if err != nil {
		t.Fatalf("Wait on finished: %v", err)
	}
	if st.State != "completed" {
		t.Errorf("state = %q", st.State)
	}
}

// =====================================================================
// Lifecycle events (live client view)
// =====================================================================

// captureSink records every event the manager publishes.
type captureSink struct {
	mu     sync.Mutex
	events []sessionstore.Event
}

func (s *captureSink) AppendDurable(_ context.Context, ev sessionstore.Event) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	return uint64(len(s.events)), nil
}

func (s *captureSink) byState(state string) []sessionstore.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []sessionstore.Event
	for _, e := range s.events {
		if e.Background != nil && e.Background.State == state {
			out = append(out, e)
		}
	}
	return out
}

func TestManager_EmitsRunningAndCompletedEvents(t *testing.T) {
	m := background.New()
	m.AttachDispatcher(instantDispatcher("done"))
	sink := &captureSink{}
	m.AttachSink(sink)

	id, err := m.Launch(context.Background(), meta.LaunchRequest{
		SessionID: "sess-x", AppID: "app-x", UserID: "u1", Tool: "database.sql",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// "running" is published synchronously at launch so the client sees
	// the task appear instantly.
	running := sink.byState("running")
	if len(running) != 1 {
		t.Fatalf("want 1 running event, got %d", len(running))
	}
	ev := running[0]
	if ev.SessionID != "sess-x" || ev.AppID != "app-x" || ev.UserID != "u1" {
		t.Errorf("running event identity wrong: %+v", ev)
	}
	if ev.Background.TaskID != id || ev.Background.Tool != "database.sql" {
		t.Errorf("running payload wrong: %+v", ev.Background)
	}

	waitFor(t, func() bool { return len(sink.byState("completed")) == 1 }, "completed event")
	if got := sink.byState("completed")[0].Background.TaskID; got != id {
		t.Errorf("completed task id = %q, want %q", got, id)
	}
}

func TestManager_EmitsErroredEvent(t *testing.T) {
	m := background.New()
	m.AttachDispatcher(&simpleDispatcher{body: func(_ context.Context, _ runtime.ToolInvocation) (string, error) {
		return "", errors.New("boom")
	}})
	sink := &captureSink{}
	m.AttachSink(sink)

	_, _ = m.Launch(context.Background(), meta.LaunchRequest{SessionID: "s", Tool: "x"})
	waitFor(t, func() bool { return len(sink.byState("errored")) == 1 }, "errored event")
	if ev := sink.byState("errored")[0]; ev.Background.Error == "" {
		t.Error("errored event should carry the failure reason")
	}
}

// recordingWaker captures proactive wake calls.
type recordingWaker struct {
	mu    sync.Mutex
	calls []string
}

func (w *recordingWaker) WakeSession(appID, sessionID, userID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, appID+"/"+sessionID+"/"+userID)
}

func TestManager_WakerCalledOnCompletion(t *testing.T) {
	m := background.New()
	m.AttachDispatcher(instantDispatcher("ok"))
	w := &recordingWaker{}
	m.AttachWaker(w)

	_, err := m.Launch(context.Background(), meta.LaunchRequest{
		SessionID: "s", AppID: "a", UserID: "u", Tool: "x",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	waitFor(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.calls) > 0
	}, "waker called on completion")

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.calls[0] != "a/s/u" {
		t.Errorf("waker identity = %q, want a/s/u", w.calls[0])
	}
}

func TestManager_EmitsCancelledEvent(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	m := background.New()
	m.AttachDispatcher(blockingDispatcher(release))
	sink := &captureSink{}
	m.AttachSink(sink)

	id, _ := m.Launch(context.Background(), meta.LaunchRequest{SessionID: "s", Tool: "x"})
	if err := m.Cancel(context.Background(), "s", id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitFor(t, func() bool { return len(sink.byState("cancelled")) == 1 }, "cancelled event")
}
