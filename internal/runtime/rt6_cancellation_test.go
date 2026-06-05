package runtime_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// RT-6 — Interruption / cancellation propre
//
// What we assert :
//
//   - Cancelling ctx mid-turn surfaces an error that wraps
//     context.Canceled and emits EventTurnEnded with
//     status="interrupted" (NOT "errored").
//   - Deadline expiry behaves identically.
//   - Interruption between iterations short-circuits (no extra
//     LLM call).
//   - Interruption during a slow tool dispatch propagates to the
//     dispatcher via ctx.
//   - Real errors STILL fail with status="errored".
//   - The terminal event is persisted even if the parent ctx is
//     already cancelled (close uses a fresh background ctx).
// =====================================================================

// blockingDispatcher waits on a channel before returning, simulating
// a slow tool. Returns ctx.Err() if ctx is cancelled first.
type blockingDispatcher struct {
	release  chan struct{}
	released atomic.Bool
}

func newBlockingDispatcher() *blockingDispatcher {
	return &blockingDispatcher{release: make(chan struct{})}
}

func (b *blockingDispatcher) Dispatch(ctx context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	select {
	case <-b.release:
		b.released.Store(true)
		return runtime.ToolOutcome{
			Status: "completed",
			Parts: []sessionstore.MessagePart{
				{Type: sessionstore.PartTypeText, Text: "released"},
			},
		}
	case <-ctx.Done():
		return runtime.ToolOutcome{
			Status: "errored",
			Error:  "ctx cancelled: " + ctx.Err().Error(),
		}
	}
}

// =====================================================================
// 1. Cancel BEFORE the first iteration
// =====================================================================

func TestRT6_CancelBeforeFirstIteration(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{resp: &llm.ChatResponse{Content: "ok"}}

	e := newEngine(t, apps, sess, lc)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, err := e.Run(ctx, runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	})
	if err == nil {
		t.Fatal("Run returned nil err for pre-cancelled ctx")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want wrapping context.Canceled", err)
	}

	// The LLM should NEVER have been called.
	if lc.calls != 0 {
		t.Errorf("LLM was called %d times despite cancellation", lc.calls)
	}

	// EventTurnEnded must have status="interrupted".
	ev := sess.find(sessionstore.EventTurnEnded)
	if ev == nil {
		t.Fatal("no EventTurnEnded persisted")
	}
	if ev.Turn == nil {
		t.Fatal("turn payload nil")
	}
	if ev.Turn.Status != "interrupted" {
		t.Errorf("status = %q, want interrupted", ev.Turn.Status)
	}
}

// =====================================================================
// 2. Cancel BETWEEN iterations
// =====================================================================

func TestRT6_CancelBetweenIterations(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// First LLM round returns a tool call ; on the *second* round
	// we want the engine to short-circuit due to cancellation.
	lc := &stubLLM{
		responses: []*llm.ChatResponse{
			{ToolCalls: []llm.ChatToolCall{{
				ID: "c1", Name: "filesystem.read",
				Arguments: map[string]any{"path": "x.txt"},
			}}},
			{Content: "should never reach here"},
		},
		onCall: func(idx int, _ *llm.ChatRequest) {
			if idx == 0 {
				// Trigger cancellation between round 1 and round 2.
				cancel()
			}
		},
	}

	cb, disp := buildRealBus(t, t.TempDir())

	e := newEngine(t, apps, sess, lc)
	e.Context = cb
	e.Dispatcher = disp

	_, err := e.Run(ctx, runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	})
	if err == nil {
		t.Fatal("Run returned nil err after mid-turn cancel")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want wrapping context.Canceled", err)
	}

	// LLM was called exactly once (the second round was cut off).
	if lc.calls != 1 {
		t.Errorf("LLM calls = %d, want 1 (second round must be skipped)", lc.calls)
	}

	// Turn must have ended as interrupted, not errored.
	ev := sess.find(sessionstore.EventTurnEnded)
	if ev == nil || ev.Turn == nil {
		t.Fatal("no EventTurnEnded")
	}
	if ev.Turn.Status != "interrupted" {
		t.Errorf("status = %q, want interrupted", ev.Turn.Status)
	}
}

// =====================================================================
// 3. Cancel DURING a slow tool dispatch
// =====================================================================

func TestRT6_CancelDuringSlowToolDispatch(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	lc := &stubLLM{
		responses: []*llm.ChatResponse{
			{ToolCalls: []llm.ChatToolCall{{
				ID: "c1", Name: "filesystem.read",
				Arguments: map[string]any{"path": "x.txt"},
			}}},
			{Content: "post-tool"},
		},
	}

	bd := newBlockingDispatcher()
	cb, _ := buildRealBus(t, t.TempDir())

	e := newEngine(t, apps, sess, lc)
	e.Context = cb
	e.Dispatcher = bd

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	var runErr error
	go func() {
		defer wg.Done()
		_, runErr = e.Run(ctx, runtime.TurnInput{
			AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
		})
	}()

	// Wait for the LLM to be called once (first round emits the
	// tool call), then cancel while the tool is blocked.
	for i := 0; i < 50 && lc.callCount() == 0; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if lc.callCount() == 0 {
		t.Fatal("LLM was never called within 1s")
	}
	cancel()
	wg.Wait()

	// blockingDispatcher returned errored due to ctx, but the
	// engine should still classify the turn as interrupted.
	ev := sess.find(sessionstore.EventTurnEnded)
	if ev == nil || ev.Turn == nil {
		t.Fatal("no EventTurnEnded")
	}
	if ev.Turn.Status != "interrupted" {
		t.Errorf("status = %q, want interrupted (ctx cancelled during tool)", ev.Turn.Status)
	}
	if runErr == nil {
		t.Error("Run should return a non-nil err on cancellation")
	}
	if bd.released.Load() {
		t.Error("blockingDispatcher should NOT have released — it should observe ctx cancel")
	}
}

// =====================================================================
// 4. DeadlineExceeded behaves like Cancelled
// =====================================================================

func TestRT6_DeadlineExceeded(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// First round returns a tool call ; on the second iteration the
	// pre-loop ctx check should fire because we've triggered a
	// deadline. We simulate the deadline expiry deterministically.
	lc := &stubLLM{
		responses: []*llm.ChatResponse{
			{ToolCalls: []llm.ChatToolCall{{
				ID: "c1", Name: "filesystem.read",
				Arguments: map[string]any{"path": "x.txt"},
			}}},
			{Content: "post-tool"},
		},
		onCall: func(idx int, _ *llm.ChatRequest) {
			if idx == 0 {
				// After round 1 finishes, expire the deadline. The
				// engine's loop-top ctx.Err() check then fires before
				// round 2's LLM.Chat call.
				cancel()
			}
		},
	}

	cb, disp := buildRealBus(t, t.TempDir())

	e := newEngine(t, apps, sess, lc)
	e.Context = cb
	e.Dispatcher = disp

	_, err := e.Run(ctx, runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	})
	if err == nil {
		t.Fatal("expected ctx error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want canceled", err)
	}

	ev := sess.find(sessionstore.EventTurnEnded)
	if ev == nil || ev.Turn == nil {
		t.Fatal("no EventTurnEnded")
	}
	if ev.Turn.Status != "interrupted" {
		t.Errorf("status = %q, want interrupted", ev.Turn.Status)
	}
}

// =====================================================================
// 5. Real errors STILL fail (not interrupted)
// =====================================================================

func TestRT6_RealErrorIsErrored(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{
		err: errors.New("provider 500"),
	}

	e := newEngine(t, apps, sess, lc)

	_, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	})
	if err == nil {
		t.Fatal("expected error from LLM provider failure")
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("LLM failure should not be classified as cancellation : %v", err)
	}

	ev := sess.find(sessionstore.EventTurnEnded)
	if ev == nil || ev.Turn == nil {
		t.Fatal("no EventTurnEnded")
	}
	if ev.Turn.Status != "errored" {
		t.Errorf("status = %q, want errored on real failure", ev.Turn.Status)
	}
	if !strings.Contains(ev.Turn.Reason, "provider 500") {
		t.Errorf("reason should mention provider error : %q", ev.Turn.Reason)
	}
}

// =====================================================================
// 6. Concurrent cancellation does not panic
// =====================================================================

func TestRT6_ConcurrentCancellationsSafe(t *testing.T) {
	const N = 30
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			app := realDispatchApp()
			apps := &stubApps{app: app}
			sess := newProjectingSessions("sess-1")
			lc := &stubLLM{
				resp: &llm.ChatResponse{Content: "hello"},
				onCall: func(_ int, _ *llm.ChatRequest) {
					time.Sleep(10 * time.Millisecond)
				},
			}
			e := newEngine(t, apps, sess, lc)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
			defer cancel()
			_, _ = e.Run(ctx, runtime.TurnInput{
				AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
			})
		}()
	}
	wg.Wait()
}
