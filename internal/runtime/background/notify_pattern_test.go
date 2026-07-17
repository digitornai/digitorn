package background

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/context/meta"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

type testWaker struct {
	mu    sync.Mutex
	calls atomic.Int32
	lastSession string
}

func (w *testWaker) WakeSession(appID, sessionID, userID string) {
	w.mu.Lock()
	w.lastSession = sessionID
	w.mu.Unlock()
	w.calls.Add(1)
}

type patternDispatcher struct {
	writeDelay time.Duration
	output     string
}

func (d *patternDispatcher) Dispatch(ctx context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	if d.writeDelay > 0 {
		select {
		case <-time.After(d.writeDelay):
		case <-ctx.Done():
			return runtime.ToolOutcome{Status: "errored", Error: "cancelled"}
		}
	}
	if sink := tool.LiveSinkFromContext(ctx); sink != nil {
		_, _ = sink.Write([]byte(d.output + "\n"))
	}
	<-ctx.Done()
	return runtime.ToolOutcome{
		Status: "errored",
		Error:  "cancelled",
		Parts:  []sessionstore.MessagePart{{Type: "text", Text: d.output}},
	}
}

func TestWatchPattern_NotifiesAgent(t *testing.T) {
	mgr := New()
	waker := &testWaker{}
	mgr.AttachWaker(waker)
	mgr.AttachDispatcher(&patternDispatcher{
		writeDelay: 200 * time.Millisecond,
		output:     "webpack compiled\nready on port 3000\nLocal:   http://localhost:3000",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tid, err := mgr.Launch(ctx, meta.LaunchRequest{
		SessionID:  "sess-notify-1",
		AppID:      "app1",
		UserID:     "user1",
		Tool:       "bash.run",
		Args:       map[string]any{"command": "npm run dev"},
		NotifyWhen: "ready on port 3000",
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Logf("launched task %s", tid)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if waker.calls.Load() > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if waker.calls.Load() == 0 {
		t.Fatal("WakeSession was never called — pattern notification did not fire")
	}
	t.Logf("WakeSession called %d time(s)", waker.calls.Load())

	notes := mgr.DrainNotifications("sess-notify-1")
	if len(notes) == 0 {
		t.Fatal("no notifications enqueued after pattern match")
	}
	n := notes[0]
	if n.Status != "pattern_matched" {
		t.Errorf("expected status=pattern_matched, got %q", n.Status)
	}
	msg := n.Message()
	if !strings.Contains(msg, "[BACKGROUND TASK READY]") {
		t.Errorf("expected [BACKGROUND TASK READY] in message:\n%s", msg)
	}
	if !strings.Contains(msg, tid) {
		t.Errorf("expected task_id %s in message:\n%s", tid, msg)
	}
	if !strings.Contains(msg, "ready on port 3000") {
		t.Errorf("expected pattern text in output:\n%s", msg)
	}
	t.Logf("notification message:\n%s", msg)

	st, err := mgr.Status(ctx, "sess-notify-1", tid)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != "running" {
		t.Errorf("expected task still running after pattern match, got %q", st.State)
	}
}

func TestWatchPattern_FiresExactlyOnce(t *testing.T) {
	mgr := New()
	waker := &testWaker{}
	mgr.AttachWaker(waker)
	mgr.AttachDispatcher(&patternDispatcher{
		writeDelay: 100 * time.Millisecond,
		output: "ready\nready\nready\nready\nready",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := mgr.Launch(ctx, meta.LaunchRequest{
		SessionID:  "sess-once",
		AppID:      "app1",
		UserID:     "user1",
		Tool:       "bash.run",
		NotifyWhen: "ready",
	})
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(1200 * time.Millisecond)

	if n := waker.calls.Load(); n != 1 {
		t.Errorf("WakeSession called %d times, want exactly 1", n)
	}
}

func TestWatchPattern_NoFalsePositive(t *testing.T) {
	mgr := New()
	waker := &testWaker{}
	mgr.AttachWaker(waker)
	mgr.AttachDispatcher(&patternDispatcher{
		writeDelay: 100 * time.Millisecond,
		output:     "server started but no magic string here",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := mgr.Launch(ctx, meta.LaunchRequest{
		SessionID:  "sess-nopattern",
		AppID:      "app1",
		UserID:     "user1",
		Tool:       "bash.run",
		NotifyWhen: "ready on port 9999",
	})
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(2 * time.Second)

	notes := mgr.DrainNotificationsPeek("sess-nopattern")
	for _, n := range notes {
		if n.Status == "pattern_matched" {
			t.Errorf("unexpected pattern_matched notification: %s", n.Message())
		}
	}
	t.Logf("no false pattern_matched notification (correct). Total notifications: %d", len(notes))
}
