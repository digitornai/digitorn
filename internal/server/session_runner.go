package server

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/mbathepaul/digitorn/internal/runtime"
)

// errTurnAborted is the cause set when a client aborts a turn. context.Cause(ctx)
// returns it so a cancelled turn says WHY it ended — otherwise a timer firing
// closes ctx.Done() without calling any cancel func, which is invisible to stack
// instrumentation (the ask_user / approval round-2 cancellation symptom). The
// safety-cutoff cause lives in the runtime package (runtime.ErrTurnSafetyCutoff)
// so the engine can recognise it and end the turn with a clear note.
var errTurnAborted = errors.New("turn aborted by client")

// sessionRunner is the single entry point for *starting an agent turn* on a
// session, from any source — a user message, a finished background task, a
// watcher, a cron reminder, an inbound channel event. It guarantees the one
// invariant the whole runtime depends on:
//
//	AT MOST ONE TURN RUNS PER SESSION AT A TIME.
//
// Two turns on the same session would interleave LLM calls and double-write
// the transcript, corrupting state. Today turns were fire-and-forget
// goroutines with no per-session guard ; that was safe only because clients
// send one message at a time. Proactive wakes (background completion, cron,
// …) break that assumption, so every trigger now funnels through here.
//
// Model : single-flight with coalescing, keyed by session id.
//   - Wake with no turn running  → start one.
//   - Wake while a turn is running → set a single "pending" flag (NOT a
//     queue — N wakes collapse to one follow-up turn). The next turn drains
//     all pending work (unprocessed user messages + background notifications)
//     at turn_start, so one coalesced turn is enough to catch up.
//   - When a turn ends with pending set → run exactly one more, then idle.
//
// Performance : no permanent goroutine per session ; a goroutine exists only
// while a session has work. State lookup is an O(1) sync.Map hit. Wake never
// blocks the caller (it takes a tiny per-session lock, flips a flag, and
// returns) — critical for callers like the background manager that wake from
// inside their own completion goroutine.
type sessionRunner struct {
	exec   func(ctx context.Context, in runtime.TurnInput) error
	cutoff time.Duration
	logger *slog.Logger

	states sync.Map // sessionID -> *sessionRunState

	// inflight maps a session to its CURRENTLY-RUNNING turn's cancel func, so
	// an abort can interrupt the live turn (not just leave a durable marker).
	// Registered for the duration of runOnce, cleared when it returns.
	cancelMu sync.Mutex
	inflight map[string]context.CancelCauseFunc
}

// sessionRunState is the per-session single-flight cell.
type sessionRunState struct {
	mu      sync.Mutex
	running bool
	pending bool
	next    runtime.TurnInput
	// lastJWT is the freshest gateway bearer seen from a user-initiated turn.
	// Proactive wakes (background completion, cron, watchers) carry no JWT of
	// their own, yet gateway mode requires one — so they reuse this. Without it
	// every proactive wake-turn fails ("gateway mode requires UserJWT") and the
	// agent is never woken to react to a finished/failed background task.
	lastJWT string
}

func newSessionRunner(
	exec func(ctx context.Context, in runtime.TurnInput) error,
	cutoff time.Duration,
	logger *slog.Logger,
) *sessionRunner {
	if cutoff <= 0 {
		cutoff = turnSafetyCutoff
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &sessionRunner{exec: exec, cutoff: cutoff, logger: logger, inflight: map[string]context.CancelCauseFunc{}}
}

// Abort interrupts the session's in-flight turn, if any, by cancelling its
// context — the engine honours ctx cancellation between iterations and on the
// live LLM call (RT-6), so the turn unwinds promptly with status="interrupted".
// Returns true if a turn was running. Safe for concurrent callers and a no-op
// when nothing is in flight.
func (r *sessionRunner) Abort(sessionID string) bool {
	if r == nil {
		return false
	}
	// Abort means STOP : drop any coalesced follow-up turn so a wake that was
	// queued while this turn ran does NOT auto-launch the moment the cancelled
	// turn unwinds. A genuinely new user message after the abort re-queues
	// normally (it sets pending again under the same lock).
	if v, ok := r.states.Load(sessionID); ok {
		st := v.(*sessionRunState)
		st.mu.Lock()
		st.pending = false
		st.next = runtime.TurnInput{}
		st.mu.Unlock()
	}
	r.cancelMu.Lock()
	cancel := r.inflight[sessionID]
	r.cancelMu.Unlock()
	if cancel == nil {
		return false
	}
	cancel(errTurnAborted)
	return true
}

func (r *sessionRunner) registerCancel(sessionID string, cancel context.CancelCauseFunc) {
	r.cancelMu.Lock()
	r.inflight[sessionID] = cancel
	r.cancelMu.Unlock()
}

func (r *sessionRunner) clearCancel(sessionID string) {
	r.cancelMu.Lock()
	delete(r.inflight, sessionID)
	r.cancelMu.Unlock()
}

// WakeTurn schedules a turn for the session in `in`, carrying whatever
// identity/credentials the caller has (a user message path supplies the
// UserJWT ; proactive paths leave it empty and run on app credentials).
// Non-blocking and safe for concurrent callers.
func (r *sessionRunner) WakeTurn(in runtime.TurnInput) {
	if r == nil || in.SessionID == "" || in.AppID == "" {
		return
	}
	v, _ := r.states.LoadOrStore(in.SessionID, &sessionRunState{})
	st := v.(*sessionRunState)

	st.mu.Lock()
	// Remember the freshest user credential; lend it to proactive wakes that
	// arrive without one (otherwise their gateway call is rejected).
	if in.UserJWT != "" {
		st.lastJWT = in.UserJWT
	} else if st.lastJWT != "" {
		in.UserJWT = st.lastJWT
	}
	if st.running {
		// A turn is already in flight — coalesce. Keep the latest identity
		// so the follow-up turn runs with fresh credentials.
		st.pending = true
		st.next = in
		st.mu.Unlock()
		return
	}
	st.running = true
	st.mu.Unlock()

	go r.loop(st, in)
}

// WakeSession is the identity-only entry point for proactive wakes
// (background completion, watchers, cron). It runs on app credentials — no
// user JWT, since there is no live user request to borrow one from.
func (r *sessionRunner) WakeSession(appID, sessionID, userID string) {
	r.WakeTurn(runtime.TurnInput{AppID: appID, SessionID: sessionID, UserID: userID})
}

// Forget drops a session's single-flight cell. Best-effort memory hygiene
// called when a session is deleted ; skips deletion if a turn is somehow
// still running so the running invariant is never violated.
func (r *sessionRunner) Forget(sessionID string) {
	if r == nil {
		return
	}
	v, ok := r.states.Load(sessionID)
	if !ok {
		return
	}
	st := v.(*sessionRunState)
	st.mu.Lock()
	running := st.running
	st.mu.Unlock()
	if !running {
		r.states.Delete(sessionID)
	}
}

// loop runs turns back-to-back while pending wakes accumulate, then idles.
// Exactly one loop goroutine exists per session at a time (guarded by the
// running flag set under st.mu before launch).
func (r *sessionRunner) loop(st *sessionRunState, in runtime.TurnInput) {
	for {
		r.runOnce(in)

		st.mu.Lock()
		if st.pending {
			st.pending = false
			in = st.next
			st.mu.Unlock()
			continue
		}
		st.running = false
		st.mu.Unlock()
		return
	}
}

// runOnce executes one turn under a fresh context guarded by a PROGRESS-based
// safety watchdog, isolating panics so a single bad turn can neither crash the
// daemon nor wedge the session's single-flight cell (the loop always returns to
// clear `running`).
//
// The watchdog is the key fix for "the turn stops right after a slow tool". A
// fixed whole-turn wall-clock deadline killed any turn whose cumulative time
// (LLM round-trips + tool executions) crossed the limit — so a productive turn
// running a slow grep, a build, or many tool rounds died mid-flight with no
// final answer. Instead, the deadline now RESETS every time the turn makes
// progress (the engine pings keepalive at each loop step). A turn that keeps
// advancing runs as long as it needs; only a turn that genuinely STALLS (no
// progress for the whole idle window) trips the cutoff — preserving the
// invariant that a wedged turn can never block its session forever.
//
// Two cancel causes so context.Cause(ctx) names the reason a turn ended :
//   - abort  : a client abort cancels with errTurnAborted.
//   - cutoff : the idle watchdog fires with runtime.ErrTurnSafetyCutoff.
//
// A plain WithTimeout would close Done() with an anonymous DeadlineExceeded and
// never call a cancel func, so instrumentation can't tell a timeout from an
// abort — the blind spot behind the ask_user round-2 cancellation.
func (r *sessionRunner) runOnce(in runtime.TurnInput) {
	defer func() {
		if rec := recover(); rec != nil {
			r.logger.Error("session_runner: turn panicked",
				slog.String("session_id", in.SessionID),
				slog.String("app_id", in.AppID),
				slog.Any("panic", rec))
		}
	}()
	base, abort := context.WithCancelCause(context.Background())
	defer abort(nil)
	// Idle-progress watchdog : fires only after r.cutoff of NO progress. Each
	// keepalive ping (one per engine loop step) reschedules it, so a turn that
	// advances never trips. time.AfterFunc + Reset is safe here — Reset is
	// called serially from the single turn-loop goroutine, and a benign race
	// with the firing callback only re-asserts an already-set cancel cause.
	watchdog := time.AfterFunc(r.cutoff, func() { abort(runtime.ErrTurnSafetyCutoff) })
	defer watchdog.Stop()
	ctx := runtime.WithTurnKeepalive(base, func() { watchdog.Reset(r.cutoff) })
	// Expose this turn's cancel so an abort can interrupt it mid-flight.
	r.registerCancel(in.SessionID, abort)
	defer r.clearCancel(in.SessionID)
	if err := r.exec(ctx, in); err != nil {
		r.logger.Error("session_runner: turn failed",
			slog.String("session_id", in.SessionID),
			slog.String("app_id", in.AppID),
			slog.String("err", err.Error()))
	}
}
