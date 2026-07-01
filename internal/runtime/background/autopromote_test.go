package background

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

type fakeDispatcher struct {
	mu     sync.Mutex
	names  []string
	bashFn func(ctx context.Context) runtime.ToolOutcome
}

func (f *fakeDispatcher) Dispatch(ctx context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	f.mu.Lock()
	f.names = append(f.names, call.Name)
	f.mu.Unlock()
	if call.Name == "bash.run" && f.bashFn != nil {
		return f.bashFn(ctx)
	}
	return okOutcome("PASSTHROUGH:" + call.Name)
}

func okOutcome(text string) runtime.ToolOutcome {
	return runtime.ToolOutcome{Status: "completed", Parts: []sessionstore.MessagePart{{Type: "text", Text: text}}}
}

func partsText(o runtime.ToolOutcome) string {
	var b strings.Builder
	for _, p := range o.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

func newTestManager(d runtime.ToolDispatcher) *Manager {
	m := New()
	m.AttachDispatcher(d)
	return m
}

// A command that finishes within the threshold returns its result transparently,
// with no promotion and no completion notification (the waiter suppresses it).
func TestPromote_FastIsTransparent(t *testing.T) {
	fd := &fakeDispatcher{bashFn: func(ctx context.Context) runtime.ToolOutcome { return okOutcome("BUILD DONE") }}
	mgr := newTestManager(fd)
	pd := NewPromotingDispatcher(fd, mgr, 500*time.Millisecond)

	out := pd.Dispatch(context.Background(), runtime.ToolInvocation{Name: "bash.run", SessionID: "s1"})
	if out.Status != "completed" {
		t.Fatalf("status = %q", out.Status)
	}
	if txt := partsText(out); txt != "BUILD DONE" {
		t.Fatalf("not transparent, got %q", txt)
	}
	if n := len(mgr.DrainNotifications("s1")); n != 0 {
		t.Fatalf("expected no notification (waiter suppresses), got %d", n)
	}
}

// A command still running at the threshold is moved to the background: the agent
// gets a handoff with the task_id immediately, and the task notifies on completion.
func TestPromote_SlowMovesToBackground(t *testing.T) {
	fd := &fakeDispatcher{bashFn: func(ctx context.Context) runtime.ToolOutcome {
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
		}
		return okOutcome("BUILD DONE LATE")
	}}
	mgr := newTestManager(fd)
	pd := NewPromotingDispatcher(fd, mgr, 60*time.Millisecond)

	start := time.Now()
	out := pd.Dispatch(context.Background(), runtime.ToolInvocation{Name: "bash.run", SessionID: "s2"})
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("Dispatch blocked %s — should return at the ~60ms threshold", elapsed)
	}
	txt := partsText(out)
	if !strings.Contains(txt, "moved to the background") || !strings.Contains(txt, "task_id") {
		t.Fatalf("expected promotion handoff, got %q", txt)
	}
	if out.Metadata["promoted"] != true {
		t.Fatalf("metadata.promoted not set: %+v", out.Metadata)
	}

	// The task keeps running and notifies when it finishes.
	deadline := time.Now().Add(2 * time.Second)
	for len(mgr.DrainNotificationsPeek("s2")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no completion notification after promotion")
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := mgr.DrainNotifications("s2")
	if len(got) != 1 {
		t.Fatalf("expected 1 completion notification, got %d", len(got))
	}
}

// A non-bash tool passes straight through to the inner dispatcher, untouched.
func TestPromote_NonBashPassthrough(t *testing.T) {
	fd := &fakeDispatcher{}
	pd := NewPromotingDispatcher(fd, newTestManager(fd), 50*time.Millisecond)
	out := pd.Dispatch(context.Background(), runtime.ToolInvocation{Name: "filesystem.read", SessionID: "s3"})
	if txt := partsText(out); txt != "PASSTHROUGH:filesystem.read" {
		t.Fatalf("not passthrough, got %q", txt)
	}
}

// An explicit background_run (ctx already marked background) is never re-promoted.
func TestPromote_AlreadyBackgroundPassthrough(t *testing.T) {
	fd := &fakeDispatcher{bashFn: func(ctx context.Context) runtime.ToolOutcome { return okOutcome("DIRECT") }}
	pd := NewPromotingDispatcher(fd, newTestManager(fd), 50*time.Millisecond)
	out := pd.Dispatch(tool.WithBackground(context.Background()), runtime.ToolInvocation{Name: "bash.run", SessionID: "s4"})
	if txt := partsText(out); txt != "DIRECT" {
		t.Fatalf("expected direct passthrough, got %q", txt)
	}
}

// A nil manager disables promotion: everything dispatches directly.
func TestPromote_NilManagerPassthrough(t *testing.T) {
	fd := &fakeDispatcher{bashFn: func(ctx context.Context) runtime.ToolOutcome { return okOutcome("PLAIN") }}
	pd := NewPromotingDispatcher(fd, nil, 50*time.Millisecond)
	out := pd.Dispatch(context.Background(), runtime.ToolInvocation{Name: "bash.run", SessionID: "s5"})
	if txt := partsText(out); txt != "PLAIN" {
		t.Fatalf("expected passthrough with nil mgr, got %q", txt)
	}
}
