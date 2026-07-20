package server

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/background"
	"github.com/digitornai/digitorn/internal/runtime/context/meta"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// bgInstantDispatcher completes immediately so a launched task finishes
// (and triggers the proactive wake) without blocking.
type bgInstantDispatcher struct{}

func (bgInstantDispatcher) Dispatch(_ context.Context, _ runtime.ToolInvocation) runtime.ToolOutcome {
	return runtime.ToolOutcome{
		Status: "completed",
		Parts:  []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: "ok"}},
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within timeout: %s", what)
}

func runnerIdle(r *sessionRunner, sid string) bool {
	v, ok := r.states.Load(sid)
	if !ok {
		return true
	}
	st := v.(*sessionRunState)
	st.mu.Lock()
	defer st.mu.Unlock()
	return !st.running && len(st.queue) == 0
}

// TestSessionRunner_ProactiveWakeReusesCachedJWT : a proactive wake (background
// completion) carries no gateway bearer of its own; it MUST reuse the JWT cached
// from the user's turns, or its LLM call is rejected ("gateway mode requires
// UserJWT") and the agent is never woken to react to a finished/failed task.
func TestSessionRunner_ProactiveWakeReusesCachedJWT(t *testing.T) {
	seen := make(chan string, 4)
	exec := func(_ context.Context, in runtime.TurnInput) error {
		seen <- in.UserJWT
		return nil
	}
	r := newSessionRunner(exec, time.Minute, testLogger())
	const sid = "s1"

	r.WakeTurn(runtime.TurnInput{AppID: "a", SessionID: sid, UserID: "u", UserJWT: "JWT-123"})
	waitUntil(t, func() bool { return runnerIdle(r, sid) }, "user turn done")
	r.WakeSession("a", sid, "u") // proactive wake: no JWT of its own
	waitUntil(t, func() bool { return runnerIdle(r, sid) }, "wake turn done")

	got1, got2 := <-seen, <-seen
	if got1 != "JWT-123" {
		t.Fatalf("user turn JWT = %q, want JWT-123", got1)
	}
	if got2 != "JWT-123" {
		t.Fatalf("proactive wake JWT = %q — it did not reuse the cached credential (gateway would reject it)", got2)
	}
}

// TestSessionRunner_AbortCancelsInFlightTurn : Abort must interrupt the running
// turn by cancelling its context, so the engine (which honours ctx between
// iterations) unwinds promptly. Without the cancel registry, abort was a no-op
// on the live turn.
func TestSessionRunner_AbortCancelsInFlightTurn(t *testing.T) {
	started := make(chan struct{})
	var ctxErr atomic.Value
	exec := func(ctx context.Context, _ runtime.TurnInput) error {
		close(started)
		<-ctx.Done() // block until cancelled by Abort
		ctxErr.Store(ctx.Err())
		return ctx.Err()
	}
	r := newSessionRunner(exec, time.Minute, testLogger())
	r.WakeTurn(runtime.TurnInput{AppID: "app", SessionID: "s1", UserID: "u"})

	<-started // turn is now running and blocked
	if !r.Abort("s1") {
		t.Fatal("Abort returned false while a turn was in flight")
	}
	waitUntil(t, func() bool { return ctxErr.Load() != nil }, "turn observed cancellation")
	if err, _ := ctxErr.Load().(error); err != context.Canceled {
		t.Errorf("turn ctx err = %v, want context.Canceled", err)
	}
	waitUntil(t, func() bool { return runnerIdle(r, "s1") }, "runner returns to idle after abort")

	// Abort with no turn running is a harmless no-op.
	if r.Abort("s1") {
		t.Error("Abort should return false when no turn is in flight")
	}
}

// TestSessionRunner_AbortDropsQueuedFollowup : abort means STOP for the machine,
// not for the human. It cancels the running turn and drops a pending PROACTIVE
// wake — "stop" must not be undone a millisecond later by a background nudge.
//
// A user message queued behind the turn is NOT dropped: it was typed and sent on
// purpose and cannot be resent from the UI. That case is covered by
// TestSessionRunner_AbortKeepsQueuedUserMessages.
func TestSessionRunner_AbortDropsQueuedFollowup(t *testing.T) {
	var execCount atomic.Int32
	started := make(chan struct{}, 1)
	exec := func(ctx context.Context, _ runtime.TurnInput) error {
		if execCount.Add(1) == 1 {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done() // the first turn blocks until aborted
			return ctx.Err()
		}
		return nil // a wrongly-run follow-up would complete here, bumping the count
	}
	r := newSessionRunner(exec, time.Minute, testLogger())
	const sid = "s1"

	r.WakeTurn(runtime.TurnInput{AppID: "a", SessionID: sid, UserID: "u"})
	<-started // turn 1 is running and blocked
	// A PROACTIVE wake lands while turn 1 runs (coalesced into pending).
	r.WakeSession("a", sid, "u")
	if !r.Abort(sid) {
		t.Fatal("Abort returned false while a turn was in flight")
	}
	waitUntil(t, func() bool { return runnerIdle(r, sid) }, "runner returns to idle after abort")

	if n := execCount.Load(); n != 1 {
		t.Fatalf("execCount = %d, want 1 — abort must DROP the pending proactive wake, not run it", n)
	}
}

// TestSessionRunner_AbortKeepsQueuedUserMessages : the user typed those while
// the agent was working. Aborting the turn must let them run, in order, until
// the queue is exhausted — dropping them loses input that cannot be resent.
func TestSessionRunner_AbortKeepsQueuedUserMessages(t *testing.T) {
	var mu sync.Mutex
	var ran []string
	started := make(chan struct{}, 1)
	block := make(chan struct{})

	exec := func(ctx context.Context, in runtime.TurnInput) error {
		mu.Lock()
		ran = append(ran, in.Skill)
		mu.Unlock()
		if in.Skill == "first" {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done() // hold until abort cancels us
			return ctx.Err()
		}
		return nil
	}

	r := newSessionRunner(exec, time.Minute, testLogger())
	const sid = "s1"

	r.WakeTurn(runtime.TurnInput{AppID: "a", SessionID: sid, UserID: "u", Skill: "first"})
	<-started
	r.WakeTurn(runtime.TurnInput{AppID: "a", SessionID: sid, UserID: "u", Skill: "second"})
	r.WakeTurn(runtime.TurnInput{AppID: "a", SessionID: sid, UserID: "u", Skill: "third"})

	if !r.Abort(sid) {
		t.Fatal("Abort returned false while a turn was in flight")
	}
	close(block)
	waitUntil(t, func() bool { return runnerIdle(r, sid) }, "runner idle after abort")

	mu.Lock()
	defer mu.Unlock()
	want := []string{"first", "second", "third"}
	if len(ran) != len(want) {
		t.Fatalf("ran = %v, want %v — abort dropped queued user messages", ran, want)
	}
	for i := range want {
		if ran[i] != want[i] {
			t.Fatalf("ran = %v, want %v (order)", ran, want)
		}
	}
}

// TestSessionRunner_SerializesAndCoalesces is the core invariant test :
// many concurrent wakes for one session must (a) never run two turns at
// once, and (b) collapse to exactly one follow-up turn (not one per wake).
func TestSessionRunner_SerializesAndCoalesces(t *testing.T) {
	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32
	var execCount atomic.Int32
	release := make(chan struct{})

	exec := func(_ context.Context, _ runtime.TurnInput) error {
		n := concurrent.Add(1)
		for {
			m := maxConcurrent.Load()
			if n <= m || maxConcurrent.CompareAndSwap(m, n) {
				break
			}
		}
		execCount.Add(1)
		<-release // hold the first turn open while the wakes pile up
		concurrent.Add(-1)
		return nil
	}

	r := newSessionRunner(exec, time.Minute, testLogger())
	in := runtime.TurnInput{AppID: "app", SessionID: "sess", UserID: "u"}

	const wakes = 20
	for i := 0; i < wakes; i++ {
		// WakeSession, not WakeTurn: coalescing is the PROACTIVE contract (a
		// burst of background completions wakes the agent once). User messages
		// go through WakeTurn and are queued individually — see
		// session_queue_test.go.
		r.WakeSession(in.AppID, in.SessionID, in.UserID)
	}

	// First turn is running ; the other 19 wakes have coalesced to pending.
	waitUntil(t, func() bool { return execCount.Load() >= 1 }, "first turn started")
	if got := maxConcurrent.Load(); got != 1 {
		t.Fatalf("max concurrent turns = %d, want 1 (serialization broken)", got)
	}

	close(release) // let the first turn finish + the coalesced follow-up run

	waitUntil(t, func() bool { return runnerIdle(r, "sess") }, "runner idle")
	if got := execCount.Load(); got != 2 {
		t.Errorf("total turns = %d, want 2 (1 initial + 1 coalesced for %d wakes)", got, wakes)
	}
	if got := maxConcurrent.Load(); got != 1 {
		t.Errorf("max concurrent turns = %d, want 1", got)
	}
}

// TestSessionRunner_DistinctSessionsRunInParallel : the serialization is
// PER SESSION — different sessions are free to run concurrently.
func TestSessionRunner_DistinctSessionsRunInParallel(t *testing.T) {
	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32
	release := make(chan struct{})

	exec := func(_ context.Context, _ runtime.TurnInput) error {
		n := concurrent.Add(1)
		for {
			m := maxConcurrent.Load()
			if n <= m || maxConcurrent.CompareAndSwap(m, n) {
				break
			}
		}
		<-release
		concurrent.Add(-1)
		return nil
	}

	r := newSessionRunner(exec, time.Minute, testLogger())
	const sessions = 8
	for i := 0; i < sessions; i++ {
		r.WakeTurn(runtime.TurnInput{AppID: "app", SessionID: string(rune('A' + i)), UserID: "u"})
	}

	waitUntil(t, func() bool { return concurrent.Load() == sessions }, "all sessions running")
	if got := maxConcurrent.Load(); got != sessions {
		t.Errorf("max concurrent = %d, want %d (distinct sessions must parallelize)", got, sessions)
	}
	close(release)
}

// TestSessionRunner_PanicIsolated : a panicking turn doesn't crash the
// daemon and doesn't wedge the session — a later wake still runs.
func TestSessionRunner_PanicIsolated(t *testing.T) {
	var calls atomic.Int32
	exec := func(_ context.Context, _ runtime.TurnInput) error {
		if calls.Add(1) == 1 {
			panic("boom in turn")
		}
		return nil
	}
	r := newSessionRunner(exec, time.Minute, testLogger())
	in := runtime.TurnInput{AppID: "app", SessionID: "sess", UserID: "u"}

	r.WakeTurn(in)
	waitUntil(t, func() bool { return runnerIdle(r, "sess") }, "first (panicking) turn settled")

	r.WakeTurn(in) // session not wedged : this must run
	waitUntil(t, func() bool { return calls.Load() >= 2 }, "second turn ran after panic")
}

// TestSessionRunner_Forget drops an idle session's cell.
func TestSessionRunner_Forget(t *testing.T) {
	r := newSessionRunner(func(_ context.Context, _ runtime.TurnInput) error { return nil }, time.Minute, testLogger())
	in := runtime.TurnInput{AppID: "app", SessionID: "sess", UserID: "u"}
	r.WakeTurn(in)
	waitUntil(t, func() bool { return runnerIdle(r, "sess") }, "turn settled")
	r.Forget("sess")
	if _, ok := r.states.Load("sess"); ok {
		t.Error("Forget did not drop the idle session state")
	}
}

// TestSessionRunner_ProactiveWakeFromBackground proves the full BG-3 chain :
// a finished background task wakes the agent through the runner, which runs
// a turn for that exact session — without any user message.
func TestSessionRunner_ProactiveWakeFromBackground(t *testing.T) {
	var ran atomic.Int32
	var lastSession atomic.Value
	r := newSessionRunner(func(_ context.Context, in runtime.TurnInput) error {
		lastSession.Store(in.SessionID)
		ran.Add(1)
		return nil
	}, time.Minute, testLogger())

	mgr := background.New()
	mgr.AttachDispatcher(bgInstantDispatcher{})
	mgr.AttachWaker(r) // the runner IS the waker

	_, err := mgr.Launch(context.Background(), meta.LaunchRequest{
		SessionID: "sess-x", AppID: "app-x", UserID: "u", Tool: "database.sql",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	waitUntil(t, func() bool { return ran.Load() >= 1 }, "proactive turn ran")
	if got, _ := lastSession.Load().(string); got != "sess-x" {
		t.Errorf("proactive turn ran for session %q, want sess-x", got)
	}
}

// TestSessionRunner_IdleWatchdogResetsOnProgress is the core proof of the fix
// for "the turn stops right after a slow tool". A turn whose total wall-clock
// time FAR exceeds the safety window survives, as long as it keeps pinging
// keepalive (the engine does this each loop step) — the watchdog resets on every
// ping and never fires. A fixed whole-turn deadline would have killed it.
func TestSessionRunner_IdleWatchdogResetsOnProgress(t *testing.T) {
	const window = 80 * time.Millisecond
	var cancelled atomic.Bool
	exec := func(ctx context.Context, _ runtime.TurnInput) error {
		// Run ~5× the window, pinging well inside it each step. A whole-turn
		// wall-clock deadline of `window` would fire ~4 times over; the
		// progress-reset watchdog must not.
		deadline := time.Now().Add(5 * window)
		for time.Now().Before(deadline) {
			runtime.PingTurnKeepalive(ctx)
			select {
			case <-ctx.Done():
				cancelled.Store(true)
				return ctx.Err()
			case <-time.After(window / 3):
			}
		}
		return nil
	}
	r := newSessionRunner(exec, window, testLogger())
	r.WakeTurn(runtime.TurnInput{AppID: "app", SessionID: "sess", UserID: "u"})
	waitUntil(t, func() bool { return runnerIdle(r, "sess") }, "progressing turn settled")
	if cancelled.Load() {
		t.Fatal("a turn that keeps making progress was killed by the idle watchdog")
	}
}

// TestSessionRunner_IdleWatchdogKillsStall : a turn that makes NO progress for
// the whole window IS killed (the safety invariant), and the cause is the named
// runtime.ErrTurnSafetyCutoff so the engine can report WHY rather than an
// anonymous "context canceled".
func TestSessionRunner_IdleWatchdogKillsStall(t *testing.T) {
	const window = 60 * time.Millisecond
	gotCause := make(chan error, 1)
	exec := func(ctx context.Context, _ runtime.TurnInput) error {
		<-ctx.Done() // never pings — a genuine stall
		gotCause <- context.Cause(ctx)
		return ctx.Err()
	}
	r := newSessionRunner(exec, window, testLogger())
	start := time.Now()
	r.WakeTurn(runtime.TurnInput{AppID: "app", SessionID: "sess", UserID: "u"})

	select {
	case cause := <-gotCause:
		if cause != runtime.ErrTurnSafetyCutoff {
			t.Fatalf("stall cancelled with cause %v, want ErrTurnSafetyCutoff", cause)
		}
		if elapsed := time.Since(start); elapsed > window*8 {
			t.Fatalf("watchdog took %v to fire (window %v) — too slow", elapsed, window)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a stalled turn was never killed by the idle watchdog")
	}
}

// TestSessionRunner_IgnoresIncompleteInput : a wake without app/session id
// is a no-op (no goroutine, no panic).
func TestSessionRunner_IgnoresIncompleteInput(t *testing.T) {
	var calls atomic.Int32
	r := newSessionRunner(func(_ context.Context, _ runtime.TurnInput) error {
		calls.Add(1)
		return nil
	}, time.Minute, testLogger())
	r.WakeTurn(runtime.TurnInput{SessionID: "sess"}) // missing AppID
	r.WakeTurn(runtime.TurnInput{AppID: "app"})      // missing SessionID
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 0 {
		t.Errorf("incomplete wakes triggered %d turns, want 0", calls.Load())
	}
}
