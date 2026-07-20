package server

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/digitornai/digitorn/internal/runtime"
)

var errTurnAborted = errors.New("turn aborted by client")

type sessionRunner struct {
	exec   func(ctx context.Context, in runtime.TurnInput) error
	cutoff time.Duration
	logger *slog.Logger

	states sync.Map

	cancelMu sync.Mutex
	inflight map[string]context.CancelCauseFunc

	// Queue side-effects, injected so the runner stays decoupled from the
	// session store and the realtime bus (and nil-safe in tests: no hook =
	// exactly the previous behaviour).
	queuedHook   func(in runtime.TurnInput, depth int)
	dequeuedHook func(in runtime.TurnInput)
}

// onQueued mirrors an enqueued turn durably and tells the session's room.
func (r *sessionRunner) onQueued(in runtime.TurnInput, depth int) {
	if r == nil || r.queuedHook == nil {
		return
	}
	r.queuedHook(in, depth)
}

// onDequeued marks the row as running just before its turn starts.
func (r *sessionRunner) onDequeued(in runtime.TurnInput) {
	if r == nil || r.dequeuedHook == nil {
		return
	}
	r.dequeuedHook(in)
}

type sessionRunState struct {
	mu      sync.Mutex
	running bool
	// Two intents, two structures — they must NOT share a slot:
	//
	//   queue  — USER messages. FIFO, never coalesced: three messages sent
	//            during a turn are three turns. The old single `next` slot
	//            overwrote itself, so only the last one ever ran.
	//   wake*  — PROACTIVE wakes (a background task finished). Coalesced on
	//            purpose: 20 completions must wake the agent ONCE, not 20
	//            times. That is the runner's long-standing core invariant.
	//
	// Both are decided under `mu`, because append and drain must be atomic
	// against `running`; the durable `message_queued` events are the mirror the
	// client reads and the daemon replays after a restart.
	queue       []runtime.TurnInput
	wakePending bool
	wakeInput   runtime.TurnInput
	lastJWT     string
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

func (r *sessionRunner) Abort(sessionID string) bool {
	if r == nil {
		return false
	}
	// Abort stops the turn in flight and drops a PENDING PROACTIVE WAKE — "stop"
	// must not be undone a millisecond later by a background nudge.
	//
	// It deliberately KEEPS the user's queued messages: those were typed and
	// sent on purpose, cannot be resent from the UI, and the loop drains them
	// until exhausted. (Previously abort discarded them along with the wake.)
	if v, ok := r.states.Load(sessionID); ok {
		st := v.(*sessionRunState)
		st.mu.Lock()
		st.wakePending = false
		st.wakeInput = runtime.TurnInput{}
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

// IsRunning reports whether a turn is currently executing for the session (a
// cancel func is registered for the whole runOnce lifetime). Used to re-arm the
// client's spinner on join/reconnect when a turn is in flight.
func (r *sessionRunner) IsRunning(sessionID string) bool {
	if r == nil {
		return false
	}
	r.cancelMu.Lock()
	_, ok := r.inflight[sessionID]
	r.cancelMu.Unlock()
	return ok
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

func (r *sessionRunner) WakeTurn(in runtime.TurnInput) {
	if r == nil || in.SessionID == "" || in.AppID == "" {
		return
	}
	v, _ := r.states.LoadOrStore(in.SessionID, &sessionRunState{})
	st := v.(*sessionRunState)

	st.mu.Lock()
	if in.UserJWT != "" {
		st.lastJWT = in.UserJWT
	} else if st.lastJWT != "" {
		in.UserJWT = st.lastJWT
	}
	if st.running {
		st.queue = append(st.queue, in)
		depth := len(st.queue)
		st.mu.Unlock()
		// Durable mirror + client notification happen OUTSIDE the lock: they do
		// I/O, and the scheduling decision above is already committed.
		r.onQueued(in, depth)
		return
	}
	st.running = true
	st.mu.Unlock()

	go r.loop(st, in)
}

// WakeSession is the PROACTIVE path (background completion, voice barge-in):
// it nudges the agent to look at new state, carries no user message, and
// coalesces — N wakes while a turn runs produce exactly one follow-up.
func (r *sessionRunner) WakeSession(appID, sessionID, userID string) {
	in := runtime.TurnInput{AppID: appID, SessionID: sessionID, UserID: userID}
	if r == nil || in.SessionID == "" || in.AppID == "" {
		return
	}
	v, _ := r.states.LoadOrStore(in.SessionID, &sessionRunState{})
	st := v.(*sessionRunState)

	st.mu.Lock()
	if in.UserJWT != "" {
		st.lastJWT = in.UserJWT
	} else if st.lastJWT != "" {
		in.UserJWT = st.lastJWT
	}
	if st.running {
		st.wakePending = true // coalesce: last one wins, no growth
		st.wakeInput = in
		st.mu.Unlock()
		return
	}
	st.running = true
	st.mu.Unlock()

	go r.loop(st, in)
}

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

func (r *sessionRunner) loop(st *sessionRunState, in runtime.TurnInput) {
	for {
		r.runOnce(in)

		st.mu.Lock()
		// User messages first, in order: they are what the user is waiting on.
		if len(st.queue) > 0 {
			in = st.queue[0]
			st.queue = st.queue[1:]
			st.mu.Unlock()
			// Same lock guards append and drain, so a message enqueued while
			// this turn was finishing can never be stranded by `running=false`.
			r.onDequeued(in)
			continue
		}
		// Then at most ONE proactive wake, whatever the number coalesced.
		if st.wakePending {
			st.wakePending = false
			in = st.wakeInput
			st.wakeInput = runtime.TurnInput{}
			st.mu.Unlock()
			continue
		}
		st.running = false
		st.mu.Unlock()
		return
	}
}

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
	watchdog := time.AfterFunc(r.cutoff, func() { abort(runtime.ErrTurnSafetyCutoff) })
	defer watchdog.Stop()
	ctx := runtime.WithTurnKeepalive(base, func() { watchdog.Reset(r.cutoff) })
	r.registerCancel(in.SessionID, abort)
	defer r.clearCancel(in.SessionID)
	if err := r.exec(ctx, in); err != nil {
		r.logger.Error("session_runner: turn failed",
			slog.String("session_id", in.SessionID),
			slog.String("app_id", in.AppID),
			slog.String("err", err.Error()))
	}
}
