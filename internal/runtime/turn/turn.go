package turn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// EventSink is the minimal slice of sessionstore.Bus the Turn needs.
// Decoupled via interface so tests inject a fake without spinning up
// the full sessionstore plumbing.
type EventSink interface {
	AppendDurable(ctx context.Context, ev sessionstore.Event) (uint64, error)
}

// IDGen mints turn IDs. Decoupled so tests can be deterministic.
// Production : uuid.NewString.
type IDGen func() string

// Options bundles construction-time dependencies. All fields except
// IDGen and Logger are required ; nil for those defaults to safe
// production wiring (uuid + slog.Default).
type Options struct {
	SessionID string
	AppID     string
	AgentID   string // which agent within the app drives this turn ; "" = unknown
	UserID    string
	UserJWT   string // forwarded to LLM in gateway mode ; ignored in BYOK
	Pool      *Pool
	Sink      EventSink
	IDGen     IDGen
	Logger    *slog.Logger
}

// Turn is one atomic unit of agent computation. It is born from a
// user message (via the orchestrator) and lives until exactly one of
// {PhaseDone, PhaseErrored, PhaseInterrupted} fires. The struct is
// NOT safe for concurrent use ; one goroutine drives it from start to
// finish.
//
// Resource invariants :
//   - Holds 1..3 pool slots (global + optional app + optional user)
//     from before Start until Close.
//   - Emits EventTurnStarted exactly once on Start.
//   - Emits EventTurnPhaseChanged once per transition.
//   - Emits EventTurnEnded exactly once on Close (idempotent).
//   - All emitted events carry the same TurnID for correlation.
type Turn struct {
	ID        string
	StepID    string
	SessionID string
	AppID     string
	AgentID   string
	UserID    string
	UserJWT   string
	StartedAt time.Time

	pool   *Pool
	token  *Token
	sink   EventSink
	logger *slog.Logger

	// mu guards the lifecycle state below. A turn dispatches its tool
	// calls in PARALLEL goroutines (dispatchToolsParallel) ; when ≥2 of
	// them hit the approval gate they each drive TransitionTo on this
	// same Turn, and a cancel can close it concurrently. The mutex makes
	// every phase transition / close serialised so the state machine is
	// data-race-free under that concurrency. Per-turn, never global.
	mu           sync.Mutex
	currentPhase Phase
	closed       bool
}

// New allocates a Turn struct with PhasePending. No event is emitted
// and no pool slot is taken yet — call Start to acquire resources and
// publish EventTurnStarted.
func New(opts Options) (*Turn, error) {
	if opts.SessionID == "" {
		return nil, errors.New("turn: SessionID required")
	}
	if opts.Pool == nil {
		return nil, errors.New("turn: Pool required")
	}
	if opts.Sink == nil {
		return nil, errors.New("turn: Sink required")
	}
	gen := opts.IDGen
	if gen == nil {
		return nil, errors.New("turn: IDGen required (production: uuid.NewString)")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Turn{
		ID:           gen(),
		SessionID:    opts.SessionID,
		AppID:        opts.AppID,
		AgentID:      opts.AgentID,
		UserID:       opts.UserID,
		UserJWT:      opts.UserJWT,
		pool:         opts.Pool,
		sink:         opts.Sink,
		logger:       logger.With(slog.String("turn_id", "")),
		currentPhase: PhasePending,
	}, nil
}

// Start acquires the pool slots (may block on saturation, returning
// ErrPoolFull if ctx expires), records StartedAt, and emits
// EventTurnStarted. After Start returns nil, the caller MUST call
// Close exactly once (via defer) regardless of subsequent error
// paths — otherwise pool slots leak.
func (t *Turn) Start(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return errors.New("turn: cannot Start after Close")
	}
	if t.token != nil {
		return errors.New("turn: Start called twice")
	}
	tok, err := t.pool.Acquire(ctx, t.AppID, t.UserID)
	if err != nil {
		return fmt.Errorf("turn: pool acquire: %w", err)
	}
	t.token = tok
	t.StartedAt = time.Now()
	t.logger = t.logger.With(slog.String("turn_id", t.ID),
		slog.String("session_id", t.SessionID), slog.String("app_id", t.AppID))

	ev := sessionstore.Event{
		Type:          sessionstore.EventTurnStarted,
		SessionID:     t.SessionID,
		AppID:         t.AppID,
		UserID:        t.UserID,
		CorrelationID: t.ID,
		Turn: &sessionstore.TurnPayload{
			TurnID:  t.ID,
			AgentID: t.AgentID,
		},
	}
	if _, err := t.sink.AppendDurable(ctx, ev); err != nil {
		// Could not record start — release the slot and surface.
		t.token.Release()
		t.token = nil
		return fmt.Errorf("turn: emit started: %w", err)
	}
	t.currentPhase = PhasePending
	t.logger.Debug("turn: started")
	return nil
}

// TransitionTo validates and emits a phase change. Returns an error
// if the transition is illegal (same-phase, backward, or skipping
// intermediates). The new phase is committed to t.currentPhase ONLY
// if both validation AND event emission succeed.
//
// For terminal transitions (Done / Errored / Interrupted), prefer
// CloseWith / Fail / Interrupt which combine the transition with
// EventTurnEnded + pool release atomically.
func (t *Turn) TransitionTo(ctx context.Context, next Phase) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return errors.New("turn: cannot TransitionTo after Close")
	}
	if t.token == nil {
		return errors.New("turn: TransitionTo before Start")
	}
	if err := Validate(t.currentPhase, next); err != nil {
		return err
	}
	ev := sessionstore.Event{
		Type:          sessionstore.EventTurnPhaseChanged,
		SessionID:     t.SessionID,
		AppID:         t.AppID,
		UserID:        t.UserID,
		CorrelationID: t.ID,
		Turn: &sessionstore.TurnPayload{
			TurnID:  t.ID,
			AgentID: t.AgentID,
			Phase:   string(next),
		},
	}
	if _, err := t.sink.AppendDurable(ctx, ev); err != nil {
		return fmt.Errorf("turn: emit phase %q: %w", next, err)
	}
	t.currentPhase = next
	t.logger.Debug("turn: phase", slog.String("phase", string(next)))
	return nil
}

// Phase returns the current phase. Safe to read concurrently.
func (t *Turn) Phase() Phase {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.currentPhase
}

// CloseDone is the success terminal. Transitions to PhaseDone (if not
// already) and emits EventTurnEnded with status="done". Releases the
// pool slot. Idempotent : second call is a no-op.
func (t *Turn) CloseDone(ctx context.Context) error {
	return t.closeWith(ctx, PhaseDone, "done", "")
}

// Fail is the error terminal. Transitions to PhaseErrored and emits
// EventTurnEnded with status="errored" + reason=err.Error(). Releases
// the pool slot. Idempotent.
//
// Use this from any error path that isn't a user-initiated
// interruption — failed LLM call, persistence error, hook panic,
// timeout, etc.
func (t *Turn) Fail(ctx context.Context, cause error) error {
	reason := ""
	if cause != nil {
		reason = cause.Error()
	}
	return t.closeWith(ctx, PhaseErrored, "errored", reason)
}

// Interrupt is the user-initiated terminal. Transitions to
// PhaseInterrupted and emits EventTurnEnded with status="interrupted".
// Idempotent. RT-6 will wire this into the abort REST endpoint.
func (t *Turn) Interrupt(ctx context.Context, reason string) error {
	return t.closeWith(ctx, PhaseInterrupted, "interrupted", reason)
}

func (t *Turn) closeWith(ctx context.Context, phase Phase, status, reason string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	// If we never Started successfully, just mark closed and stop.
	// The caller probably already surfaced the start error.
	if t.token == nil {
		t.closed = true
		return nil
	}
	// Validate the phase change locally but DON'T emit a separate
	// PhaseChanged event for terminal transitions : EventTurnEnded
	// already carries (status, reason) which fully replaces what a
	// PhaseChanged event would say. Emitting both would double-publish
	// the same information and confuse consumers counting events per
	// turn. Pre-condition : current phase must be able to legally
	// reach the terminal phase (every non-terminal can).
	if !t.currentPhase.IsTerminal() {
		if err := Validate(t.currentPhase, phase); err != nil {
			t.releaseAndMark()
			return fmt.Errorf("turn: closeWith validate: %w", err)
		}
		t.currentPhase = phase
	}
	ev := sessionstore.Event{
		Type:          sessionstore.EventTurnEnded,
		SessionID:     t.SessionID,
		AppID:         t.AppID,
		UserID:        t.UserID,
		CorrelationID: t.ID,
		Turn: &sessionstore.TurnPayload{
			TurnID:  t.ID,
			AgentID: t.AgentID,
			Phase:   string(phase),
			Status:  status,
			Reason:  reason,
		},
	}
	emitErr := error(nil)
	if _, err := t.sink.AppendDurable(ctx, ev); err != nil {
		emitErr = fmt.Errorf("turn: emit ended: %w", err)
	}
	t.releaseAndMark()
	t.logger.Debug("turn: closed",
		slog.String("status", status),
		slog.Duration("duration", time.Since(t.StartedAt)))
	return emitErr
}

func (t *Turn) releaseAndMark() {
	if t.token != nil {
		t.token.Release()
		t.token = nil
	}
	t.closed = true
}
