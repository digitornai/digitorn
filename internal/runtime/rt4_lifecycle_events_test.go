package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/llm"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/hooks"
	"github.com/digitornai/digitorn/internal/runtime/policy/approval"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// =====================================================================
// Lifecycle hook events — these used to COMPILE but NEVER fire in the
// production runtime (session_start, session_end, pre_compact, error,
// approval_request). These tests lock that they now fire at the
// documented moments (docs-site/language/31-tool-hooks.md "The events").
// =====================================================================

// appendUserMsg appends a user message so the session's TurnCount
// advances exactly as it does in production (projection increments
// TurnCount on EventUserMessage).
func appendUserMsg(t *testing.T, sess *projectingSessions, sid, content string) {
	t.Helper()
	if _, err := sess.AppendDurable(context.Background(), sessionstore.Event{
		Type:      sessionstore.EventUserMessage,
		SessionID: sid,
		Message:   &sessionstore.MessagePayload{Role: "user", Content: content},
	}); err != nil {
		t.Fatalf("append user msg: %v", err)
	}
}

// TestRT4Lifecycle_SessionStartFiresFirstTurnOnly : session_start fires
// on the first turn (turn == 0 / TurnCount <= 1) and NOT on later turns.
func TestRT4Lifecycle_SessionStartFiresFirstTurnOnly(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-ss")

	logger := &rt4Logger{}
	eng := hooks.New([]schema.Hook{
		makeLogHook("ss", schema.HookEventSessionStart, "marker_ss"),
	}, hooks.ActionDeps{Logger: logger})
	eng.Async = false

	lc := &stubLLM{resp: &llm.ChatResponse{Content: "ok"}}
	e := newEngine(t, apps, sess, lc)
	e.Hooks = &hookSourceWith{eng: eng}

	// First turn.
	appendUserMsg(t, sess, "sess-ss", "hello one")
	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-ss", UserID: "u",
	}); err != nil {
		t.Fatalf("Run #1: %v", err)
	}
	if got := logger.count("marker_ss"); got != 1 {
		t.Fatalf("session_start fired %d times on first turn, want 1", got)
	}

	// Second turn — session_start must NOT fire again.
	appendUserMsg(t, sess, "sess-ss", "hello two")
	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-ss", UserID: "u",
	}); err != nil {
		t.Fatalf("Run #2: %v", err)
	}
	if got := logger.count("marker_ss"); got != 1 {
		t.Errorf("session_start fired again on second turn (count=%d, want 1)", got)
	}
}

// TestRT4Lifecycle_ErrorHookFiresWithErrorType : when the LLM call fails
// the error hook fires, and the ErrorType payload is fed so an
// error_type-conditioned hook (regex) matches.
func TestRT4Lifecycle_ErrorHookFiresWithErrorType(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-err")

	logger := &rt4Logger{}
	hk := schema.Hook{
		ID:        "er",
		On:        schema.HookEventError,
		Condition: schema.HookCondition{Type: "error_type", Params: map[string]any{"match": "llm boom"}},
		Action:    schema.HookAction{Type: "log", Params: map[string]any{"message": "marker_err"}},
	}
	eng := hooks.New([]schema.Hook{hk}, hooks.ActionDeps{Logger: logger})
	eng.Async = false

	lc := &stubLLM{err: errors.New("llm boom")}
	e := newEngine(t, apps, sess, lc)
	e.Hooks = &hookSourceWith{eng: eng}

	if _, err := e.Run(context.Background(), runtime.TurnInput{
		AppID: "rt3-app", SessionID: "sess-err", UserID: "u",
	}); err == nil {
		t.Fatal("expected Run to return the LLM error")
	}
	if got := logger.count("marker_err"); got != 1 {
		t.Errorf("error hook (error_type=llm boom) fired %d times, want 1 — ErrorType not fed or event not emitted", got)
	}
}

// TestRT4Lifecycle_FireLifecycle_SessionEndAndPreCompact : the engine's
// FireLifecycle (used by the daemon's session-delete and compaction
// handlers) fires session_end and pre_compact through the SAME engine.
func TestRT4Lifecycle_FireLifecycle_SessionEndAndPreCompact(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-lc")

	logger := &rt4Logger{}
	eng := hooks.New([]schema.Hook{
		makeLogHook("se", schema.HookEventSessionEnd, "marker_se"),
		makeLogHook("pc", schema.HookEventPreCompact, "marker_pc"),
	}, hooks.ActionDeps{Logger: logger})
	eng.Async = false

	lc := &stubLLM{resp: &llm.ChatResponse{Content: "ok"}}
	e := newEngine(t, apps, sess, lc)
	e.Hooks = &hookSourceWith{eng: eng}

	e.FireLifecycle(context.Background(), schema.HookEventSessionEnd, "rt3-app", "sess-lc", "u")
	e.FireLifecycle(context.Background(), schema.HookEventPreCompact, "rt3-app", "sess-lc", "u")

	if got := logger.count("marker_se"); got != 1 {
		t.Errorf("session_end fired %d times, want 1", got)
	}
	if got := logger.count("marker_pc"); got != 1 {
		t.Errorf("pre_compact fired %d times, want 1", got)
	}
}

// TestRT4Lifecycle_FireLifecycle_NilSafe : FireLifecycle on an engine
// with no hook source is a clean no-op.
func TestRT4Lifecycle_FireLifecycle_NilSafe(t *testing.T) {
	app := realDispatchApp()
	apps := &stubApps{app: app}
	sess := newProjectingSessions("sess-nil")
	lc := &stubLLM{resp: &llm.ChatResponse{Content: "ok"}}
	e := newEngine(t, apps, sess, lc)
	e.Hooks = nil

	// Must not panic.
	_ = e.FireLifecycle(context.Background(), schema.HookEventSessionEnd, "rt3-app", "sess-nil", "u")
}

// TestRT4Lifecycle_ApprovalRequestHookFires : when a tool call hits the
// approval gate, the approval_request hook fires as the request is
// enqueued. Reuses the SG-5 approval harness.
func TestRT4Lifecycle_ApprovalRequestHookFires(t *testing.T) {
	s := buildApprovalScenario(t, []string{"bash"}, 60)

	logger := &rt4Logger{}
	eng := hooks.New([]schema.Hook{
		makeLogHook("ar", schema.HookEventApprovalRequest, "marker_approval"),
	}, hooks.ActionDeps{Logger: logger})
	eng.Async = false
	s.e.Hooks = &hookSourceWith{eng: eng}

	done := make(chan error, 1)
	go func() {
		_, err := s.e.Run(context.Background(), runtime.TurnInput{
			AppID: "app-1", SessionID: "sess-1", UserID: "user-A",
		})
		done <- err
	}()

	// Wait for the approval_request hook to fire.
	deadline := time.Now().Add(2 * time.Second)
	for logger.count("marker_approval") == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if logger.count("marker_approval") == 0 {
		t.Fatal("approval_request hook did not fire when the gate enqueued a request")
	}

	// Resolve so the turn goroutine unblocks instead of waiting the full
	// (clamped 30s) approval timeout.
	if ev := s.sess.findAppend(sessionstore.EventApprovalRequest); ev != nil && ev.Approval != nil {
		s.registry.Resolve(ev.Approval.ID, approval.Resolution{Result: approval.ResultApproved})
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not finish after approval resolution")
	}
}
