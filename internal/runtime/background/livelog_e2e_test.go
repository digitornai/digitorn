package background_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/background"
	"github.com/mbathepaul/digitorn/internal/runtime/context/meta"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// streamingDispatcher reproduces what the bash detached runner does in
// production: it writes lines to the live sink the manager attached via
// context, then signals completion. The agent watching this task should see
// those lines appear in Status() while the task is still running — and
// tail_lines should slice them to exactly the most recent N.
type streamingDispatcher struct {
	lines    []string
	cadence  time.Duration
	released atomic.Bool
}

func (s *streamingDispatcher) Dispatch(ctx context.Context, _ runtime.ToolInvocation) runtime.ToolOutcome {
	sink := tool.LiveSinkFromContext(ctx)
	for i, line := range s.lines {
		select {
		case <-ctx.Done():
			return runtime.ToolOutcome{Status: "cancelled", Error: ctx.Err().Error()}
		default:
		}
		if sink != nil {
			_, _ = sink.Write([]byte(line + "\n"))
		}
		if i < len(s.lines)-1 && s.cadence > 0 {
			time.Sleep(s.cadence)
		}
	}
	s.released.Store(true)
	return runtime.ToolOutcome{
		Status: "completed",
		Parts: []sessionstore.MessagePart{
			{Type: sessionstore.PartTypeText, Text: "done"},
		},
	}
}

// TestLiveLog_E2E_StatusReturnsLiveOutputWhileRunning is the production-path
// proof: launch a task that streams lines, then call Status mid-run and
// confirm the running task's log carries the lines that have already been
// emitted. This is the exact behaviour the agent relies on to know whether a
// background build / install / dev server is making progress.
func TestLiveLog_E2E_StatusReturnsLiveOutputWhileRunning(t *testing.T) {
	disp := &streamingDispatcher{
		lines: []string{
			"npm install: resolving dependencies...",
			"npm install: 100 packages added",
			"npm install: build complete",
			"npm install: starting tests",
			"npm install: 12 / 50 tests passed",
		},
		cadence: 80 * time.Millisecond,
	}

	mgr := background.New()
	mgr.AttachDispatcher(disp)

	taskID, err := mgr.Launch(context.Background(), meta.LaunchRequest{
		SessionID: "s1",
		Tool:      "bash.run",
		Args:      map[string]any{"command": "fake"},
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// Poll Status until the running task has at least 3 of the 5 streamed lines
	// — proves the live sink is being drained into the log buffer while the
	// task is still in flight, NOT only after completion.
	var st meta.BackgroundStatus
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, err = mgr.Status(context.Background(), "s1", taskID)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if st.Log != "" && strings.Count(st.Log, "npm install:") >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(st.Log, "npm install: 100 packages added") {
		t.Fatalf("live log did not surface the 2nd streamed line.\nstate=%q log=%q", st.State, st.Log)
	}
	t.Logf("OK — mid-run Status carried %d live lines:\n%s",
		strings.Count(st.Log, "\n"), st.Log)

	// Final Wait so the task drains fully and the result is visible.
	final, err := mgr.Wait(context.Background(), "s1", taskID, 5)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if final.State != "completed" {
		t.Errorf("final state=%q want completed", final.State)
	}
	// At completion the rolling log should still hold the last lines of output.
	if !strings.Contains(final.Log, "npm install: 12 / 50 tests passed") {
		t.Errorf("post-completion log lost the last streamed line: %q", final.Log)
	}
}
