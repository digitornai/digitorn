package background_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/background"
)

// panicDispatcher models a module whose handler panics (nil deref, bad type
// assertion, a worker-side decode crash).
type panicDispatcher struct{}

func (panicDispatcher) Dispatch(_ context.Context, _ runtime.ToolInvocation) runtime.ToolOutcome {
	panic("boom in module handler")
}

// TestManager_DispatcherPanic_NoCrash : a panic in the dispatched tool must NOT
// crash the daemon (the test process surviving is the proof) and must NOT leave
// the task pinned "running" forever — it is marked errored and a notification
// carrying the panic reason is enqueued so the agent learns it died.
func TestManager_DispatcherPanic_NoCrash(t *testing.T) {
	m := background.New()
	m.AttachDispatcher(panicDispatcher{})

	id, err := launch(m, "s", "x.y", nil)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	waitFor(t, func() bool { return len(m.DrainNotificationsPeek("s")) > 0 }, "panicked task settled")

	if st, _ := m.Status(context.Background(), "s", id); st.State != "errored" {
		t.Fatalf("panicked task must be errored (not pinned running), got %q", st.State)
	}
	n := m.DrainNotifications("s")[0]
	if n.Status != "errored" {
		t.Fatalf("notification status = %q, want errored", n.Status)
	}
	if !strings.Contains(n.Message(), "panic") {
		t.Fatalf("notification must carry the panic reason: %q", n.Message())
	}
}
