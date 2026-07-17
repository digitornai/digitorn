package background_test

import (
	"context"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/background"
)

type panicDispatcher struct{}

func (panicDispatcher) Dispatch(_ context.Context, _ runtime.ToolInvocation) runtime.ToolOutcome {
	panic("boom in module handler")
}

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
