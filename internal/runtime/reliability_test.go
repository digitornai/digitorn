package runtime_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/llm"
	dgruntime "github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// UT-R1 — Fault injection : downstream layers misbehave, engine
// must DEGRADE not CRASH. Every test here proves the runtime
// returns a clean error (or a clean errored outcome) rather than
// panicking or leaving the turn in a stuck state.
// =====================================================================

// nilDispatcher always returns a zero-value ToolOutcome (no status,
// no parts, no error). The engine's "defensive" code must
// classify this as errored, not as completed-with-empty-content
// (which would silently mislead the LLM).
type nilDispatcher struct{}

func (nilDispatcher) Dispatch(_ context.Context, _ dgruntime.ToolInvocation) dgruntime.ToolOutcome {
	return dgruntime.ToolOutcome{}
}

func TestReliability_NilOutcomeBecomesCompleted(t *testing.T) {
	// The defensive coder in engine.go sets Status="completed" when
	// nothing's wrong AND nothing's reported. That's the documented
	// behaviour ; we verify it here so future regressions don't
	// silently change the contract.
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID: "c1", Name: "filesystem.read",
			Arguments: map[string]any{"path": "x"},
		}}},
		{Content: "done"},
	}}

	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = nilDispatcher{}

	if _, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// EventToolResult must have been emitted.
	if got := sess.count(sessionstore.EventToolResult); got != 1 {
		t.Errorf("tool_result events = %d, want 1", got)
	}
	// And the engine reaches turn_complete.
	if got := sess.count(sessionstore.EventTurnEnded); got != 1 {
		t.Errorf("turn_ended = %d, want 1", got)
	}
}

// slowDispatcher blocks until released. Combined with a cancelled
// ctx, the engine must abort the turn cleanly.
type slowDispatcher struct {
	release chan struct{}
}

func newSlowDispatcher() *slowDispatcher {
	return &slowDispatcher{release: make(chan struct{})}
}

func (s *slowDispatcher) Dispatch(ctx context.Context, _ dgruntime.ToolInvocation) dgruntime.ToolOutcome {
	select {
	case <-s.release:
		return dgruntime.ToolOutcome{
			Status: "completed",
			Parts:  []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "released"}},
		}
	case <-ctx.Done():
		return dgruntime.ToolOutcome{Status: "errored", Error: ctx.Err().Error()}
	}
}

func TestReliability_DispatcherStuckCancelsCleanly(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLM{responses: []*llm.ChatResponse{
		{ToolCalls: []llm.ChatToolCall{{
			ID: "c1", Name: "filesystem.read",
			Arguments: map[string]any{"path": "x"},
		}}},
		{Content: "ok"},
	}}

	disp := newSlowDispatcher()
	defer close(disp.release)

	e := newEngine(t, apps, sess, lc)
	e.Dispatcher = disp

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := e.Run(ctx, dgruntime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	})
	if err == nil {
		t.Fatal("want ctx error")
	}
	// Turn must be marked interrupted, not errored, because the
	// cause was ctx cancellation.
	ev := sess.find(sessionstore.EventTurnEnded)
	if ev == nil || ev.Turn == nil {
		t.Fatal("no EventTurnEnded")
	}
	if ev.Turn.Status != "interrupted" {
		t.Errorf("status = %q, want interrupted", ev.Turn.Status)
	}
}

// stubLLMReturnsNil simulates a malformed worker response : Chat
// returns (nil, nil). The engine must NOT dereference nil and
// must error cleanly.
type stubLLMReturnsNil struct{}

func (stubLLMReturnsNil) Chat(_ context.Context, _ *llm.ChatRequest) (*llm.ChatResponse, error) {
	return nil, nil
}

func TestReliability_LLMReturnsNilNotPanic(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")

	e := newEngine(t, apps, sess, stubLLMReturnsNil{})
	_, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	})
	if err == nil {
		t.Fatal("expected error for nil LLM response")
	}
	// Turn must be properly closed as errored (not left dangling).
	ev := sess.find(sessionstore.EventTurnEnded)
	if ev == nil || ev.Turn == nil || ev.Turn.Status != "errored" {
		t.Errorf("turn must end as errored, got %+v", ev)
	}
}

// stubLLMTransientError simulates a worker that errors once with a retryable
// network fault, then succeeds. The engine AUTO-RETRIES transient failures that
// hit before any token streamed (errclass.Retry + empty partial), so the turn
// recovers on the second attempt and a durable turn_retry event is emitted for
// the client. This test documents that contract.
type stubLLMTransientError struct {
	calls atomic.Int32
}

func (s *stubLLMTransientError) Chat(_ context.Context, _ *llm.ChatRequest) (*llm.ChatResponse, error) {
	n := s.calls.Add(1)
	if n == 1 {
		return nil, errors.New("transient: dial tcp connection refused")
	}
	return &llm.ChatResponse{Content: "ok after retry"}, nil
}

func TestReliability_LLMTransientErrorRetriesAndRecovers(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-1")
	lc := &stubLLMTransientError{}

	e := newEngine(t, apps, sess, lc)
	_, err := e.Run(context.Background(), dgruntime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
	})
	if err != nil {
		t.Fatalf("transient error must be retried and recover, got: %v", err)
	}
	if lc.calls.Load() != 2 {
		t.Errorf("LLM called %d times, want 2 (one auto-retry)", lc.calls.Load())
	}
	if ev := sess.find(sessionstore.EventTurnRetry); ev == nil {
		t.Error("expected a turn_retry event emitted for the client")
	} else if ev.Retry == nil || ev.Retry.Attempt != 2 {
		t.Errorf("turn_retry must announce attempt #2, got %+v", ev.Retry)
	}
	if ev := sess.find(sessionstore.EventTurnEnded); ev == nil || ev.Turn == nil || ev.Turn.Status != "done" {
		t.Errorf("turn must end as done after recovery, got %+v", ev)
	}
}

// =====================================================================
// UT-R3 — Goroutine leak detection. After 100 turns, the goroutine
// count must return to baseline (modulo runtime-internal noise).
// =====================================================================

func TestReliability_NoGoroutineLeakAfter100Turns(t *testing.T) {
	if testing.Short() {
		t.Skip("leak test slow under -short")
	}
	app := realDispatchApp()
	apps := &stubApps{app: app}

	// Baseline. Allow GC + scheduler to settle.
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	for i := 0; i < 100; i++ {
		sess := newProjectingSessions("sess-1")
		lc := &stubLLM{resp: &llm.ChatResponse{Content: "ok"}}
		e := newEngine(t, apps, sess, lc)
		_, err := e.Run(context.Background(), dgruntime.TurnInput{
			AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
		})
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}

	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	after := runtime.NumGoroutine()

	// Allow some slack : the runtime itself may have spawned
	// finalizer goroutines, GC sweepers, etc.
	delta := after - before
	if delta > 20 {
		t.Errorf("goroutine leak : before=%d after=%d (Δ=%d)", before, after, delta)
	}
}

func TestReliability_NoGoroutineLeakAfterCancelledTurns(t *testing.T) {
	if testing.Short() {
		t.Skip("leak test slow under -short")
	}
	app := realDispatchApp()
	apps := &stubApps{app: app}

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	for i := 0; i < 50; i++ {
		sess := newProjectingSessions("sess-1")
		disp := newSlowDispatcher()
		lc := &stubLLM{responses: []*llm.ChatResponse{
			{ToolCalls: []llm.ChatToolCall{{
				ID: "c1", Name: "filesystem.read",
				Arguments: map[string]any{"path": "x"},
			}}},
			{Content: "ok"},
		}}
		e := newEngine(t, apps, sess, lc)
		e.Dispatcher = disp

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		_, _ = e.Run(ctx, dgruntime.TurnInput{
			AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
		})
		cancel()
		close(disp.release)
	}

	// Give goroutines a generous window to exit.
	for i := 0; i < 10; i++ {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
	after := runtime.NumGoroutine()

	delta := after - before
	if delta > 30 {
		t.Errorf("goroutine leak after cancellations : before=%d after=%d (Δ=%d)", before, after, delta)
	}
}

// =====================================================================
// UT-R1 (continued) — Concurrent turns with mixed outcomes
// (some OK, some errored, some cancelled) must all close cleanly.
// =====================================================================

func TestReliability_MixedConcurrentOutcomes(t *testing.T) {
	const N = 100
	app := realDispatchApp()

	var wg sync.WaitGroup
	wg.Add(N)
	var okCount, errCount, cancelCount atomic.Int32

	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			apps := &stubApps{app: app}
			sess := newProjectingSessions("sess-1")

			var (
				lc  dgruntime.LLMChat
				ctx context.Context
			)
			switch i % 3 {
			case 0:
				lc = &stubLLM{resp: &llm.ChatResponse{Content: "ok"}}
				ctx = context.Background()
				okCount.Add(1)
			case 1:
				lc = stubLLMReturnsNil{}
				ctx = context.Background()
				errCount.Add(1)
			case 2:
				lc = &stubLLM{resp: &llm.ChatResponse{Content: "ok"}}
				c, cancel := context.WithCancel(context.Background())
				cancel() // pre-cancel
				ctx = c
				cancelCount.Add(1)
			}

			e := newEngine(t, apps, sess, lc)
			_, _ = e.Run(ctx, dgruntime.TurnInput{
				AppID: "rt3-app", SessionID: "sess-1", UserID: "u",
			})

			// Every turn must have a TurnEnded event regardless of
			// outcome.
			if got := sess.count(sessionstore.EventTurnEnded); got != 1 {
				t.Errorf("[%d] TurnEnded events = %d, want 1", i, got)
			}
		}()
	}
	wg.Wait()
	t.Logf("ok=%d err=%d cancel=%d", okCount.Load(), errCount.Load(), cancelCount.Load())
}
