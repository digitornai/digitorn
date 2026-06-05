package runtime_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// overflowThenOKLLM rejects the first call with a context-overflow error, then
// succeeds — the exact shape the engine's emergency recovery must handle.
type overflowThenOKLLM struct {
	mu    sync.Mutex
	calls int
}

func (l *overflowThenOKLLM) Chat(_ context.Context, _ *llm.ChatRequest) (*llm.ChatResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.calls == 1 {
		return nil, errors.New("This model's maximum context length is 8192 tokens")
	}
	return &llm.ChatResponse{Content: "recovered", Model: "stub"}, nil
}

func (l *overflowThenOKLLM) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

type alwaysErrLLM struct{ err error }

func (l *alwaysErrLLM) Chat(_ context.Context, _ *llm.ChatRequest) (*llm.ChatResponse, error) {
	return nil, l.err
}

// recordingCompactor implements runtime.EmergencyCompactor : it records the
// emergency call and lays down a real compaction marker so the engine's reload
// sees a smaller prompt on retry.
type recordingCompactor struct {
	sess   *projectingSessions
	mu     sync.Mutex
	called int
}

func (c *recordingCompactor) CompactSession(ctx context.Context, sessionID, strategy string, keepLast int) error {
	c.mu.Lock()
	c.called++
	c.mu.Unlock()
	_, err := c.sess.AppendDurable(ctx, sessionstore.Event{
		Type:      sessionstore.EventContextCompacted,
		SessionID: sessionID,
		CtxCompact: &sessionstore.ContextCompactPayload{
			CutoffSeq: 1, Summary: "EMERGENCY-SUMMARY", KeepRecent: keepLast, Strategy: strategy,
		},
	})
	return err
}

func (c *recordingCompactor) timesCalled() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.called
}

// TestCTX5_EmergencyCompactRecoversOverflow proves the emergency path end to
// end: the first LLM call overflows, the engine compacts the session and
// retries ONCE, and the turn succeeds instead of failing.
func TestCTX5_EmergencyCompactRecoversOverflow(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-emerg")
	ctx := context.Background()

	appendUserMsg(t, sess, "sess-emerg", "MSG-1")
	appendUserMsg(t, sess, "sess-emerg", "MSG-2")

	lc := &overflowThenOKLLM{}
	e := newEngine(t, apps, sess, lc)
	comp := &recordingCompactor{sess: sess}
	e.Compactor = comp

	if _, err := e.Run(ctx, runtime.TurnInput{AppID: "rt3-app", SessionID: "sess-emerg", UserID: "u"}); err != nil {
		t.Fatalf("turn failed despite emergency recovery: %v", err)
	}
	if got := comp.timesCalled(); got != 1 {
		t.Fatalf("emergency compaction triggered %d times, want exactly 1", got)
	}
	if got := lc.count(); got != 2 {
		t.Fatalf("expected 2 LLM calls (overflow + retry), got %d", got)
	}
}

// TestCTX5_NonOverflowErrorNotRetried : a non-overflow error must NOT trigger
// emergency compaction nor a retry — it propagates as the turn failure.
func TestCTX5_NonOverflowErrorNotRetried(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-emerg2")
	ctx := context.Background()
	appendUserMsg(t, sess, "sess-emerg2", "MSG-1")

	lc := &alwaysErrLLM{err: errors.New("invalid api key")}
	e := newEngine(t, apps, sess, lc)
	comp := &recordingCompactor{sess: sess}
	e.Compactor = comp

	if _, err := e.Run(ctx, runtime.TurnInput{AppID: "rt3-app", SessionID: "sess-emerg2", UserID: "u"}); err == nil {
		t.Fatal("expected the turn to fail on a non-overflow error")
	}
	if got := comp.timesCalled(); got != 0 {
		t.Fatalf("compaction must NOT fire on a non-overflow error (called=%d)", got)
	}
}
