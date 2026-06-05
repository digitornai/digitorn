// Package turn implements the foundational Turn abstraction that every
// runtime feature (tool dispatch, hooks, approvals, multi-agent) is
// built on top of. A Turn is the atomic unit of agent computation : it
// is born from a user message, holds resources (1 goroutine + 1 pool
// slot at each of 3 tiers) for the duration of one chat-completion
// cycle, persists every state transition durably as an event, and is
// freed back to the pool when terminal.
//
// Crash safety : a Turn never keeps its lifecycle in a private map.
// All state is in the sessionstore as Turn* events ; killing the daemon
// mid-turn is recoverable because the next boot can detect "phase ∈
// {loading, running, persisting}" sessions and surface an error to the
// caller.
package turn

import "fmt"

// Phase enumerates the discrete states a Turn moves through. Adding a
// new phase is a breaking change : update the transition matrix below
// AND every event consumer that filters by phase.
type Phase string

const (
	// PhasePending is the initial state set when the Turn struct is
	// allocated but no event has been emitted yet. Should be transient
	// (microseconds) — only useful as a sentinel for "freshly built".
	PhasePending Phase = "pending"

	// PhaseLoading covers app lookup + session snapshot + message
	// projection. Pre-LLM work that can fail synchronously.
	PhaseLoading Phase = "loading"

	// PhaseRunning is the LLM call in flight. By far the longest
	// phase (seconds). Cancellation honored via ctx.Done().
	PhaseRunning Phase = "running"

	// PhaseWaitingApproval covers the synchronous pause described in
	// docs-site/docs/tutorial/security-01-approval.md : when gate 4
	// resolves to "approve" for a tool_call, the turn suspends here
	// until a human resolves the approval (or the timeout expires).
	// No tokens are billed and no LLM call happens during this phase.
	// The phase transitions back to PhaseRunning once resolved.
	PhaseWaitingApproval Phase = "waiting_approval"

	// PhasePersisting covers AppendDurable for the assistant message
	// and any final cost/usage events. Short (ms-scale) but the only
	// phase where a daemon crash partially commits.
	PhasePersisting Phase = "persisting"

	// PhaseDone is terminal-success.
	PhaseDone Phase = "done"

	// PhaseErrored is terminal-failure.
	PhaseErrored Phase = "errored"

	// PhaseInterrupted is terminal-cancelled (user clicked stop OR
	// daemon shutdown OR ctx deadline).
	PhaseInterrupted Phase = "interrupted"
)

// IsTerminal reports whether the phase is a final state. Terminal
// phases must trigger pool release and EventTurnEnded emission.
func (p Phase) IsTerminal() bool {
	switch p {
	case PhaseDone, PhaseErrored, PhaseInterrupted:
		return true
	}
	return false
}

// IsInFlight reports whether a turn in this phase was actively doing
// work at the time of observation. Used by recovery to detect turns
// that need an EventError{daemon_restarted} marker at boot.
func (p Phase) IsInFlight() bool {
	switch p {
	case PhaseLoading, PhaseRunning, PhaseWaitingApproval, PhasePersisting:
		return true
	}
	return false
}

// validTransitions encodes the lifecycle. Any transition not in the
// matrix is rejected by CanTransition / Validate ; this is the
// runtime's safety net against misuse (e.g. emitting PhaseChanged out
// of order).
//
//	pending           → loading | errored | interrupted
//	loading           → running | errored | interrupted
//	running           → persisting | waiting_approval | errored | interrupted
//	waiting_approval  → running | errored | interrupted
//	persisting        → done | errored | interrupted
//	done | errored | interrupted → (none — terminal)
var validTransitions = map[Phase]map[Phase]bool{
	PhasePending: {
		PhaseLoading:     true,
		PhaseErrored:     true,
		PhaseInterrupted: true,
	},
	PhaseLoading: {
		PhaseRunning:     true,
		PhaseErrored:     true,
		PhaseInterrupted: true,
	},
	PhaseRunning: {
		PhasePersisting:      true,
		PhaseWaitingApproval: true,
		PhaseErrored:         true,
		PhaseInterrupted:     true,
	},
	PhaseWaitingApproval: {
		PhaseRunning:     true,
		PhaseErrored:     true,
		PhaseInterrupted: true,
	},
	PhasePersisting: {
		PhaseDone:        true,
		PhaseErrored:     true,
		PhaseInterrupted: true,
	},
	PhaseDone:        {},
	PhaseErrored:     {},
	PhaseInterrupted: {},
}

// CanTransition reports whether moving from `from` to `to` is allowed.
// Same-phase transitions are NOT allowed (idempotency is the caller's
// responsibility — emitting two PhaseChanged for the same phase is a
// bug).
func CanTransition(from, to Phase) bool {
	allowed, ok := validTransitions[from]
	if !ok {
		return false
	}
	return allowed[to]
}

// Validate returns nil if the transition is legal, otherwise an error
// suitable for surfacing in logs / EventError. Useful when a higher
// layer wants to fail loudly instead of silently dropping a bad event.
func Validate(from, to Phase) error {
	if from == to {
		return fmt.Errorf("turn: no-op transition %q→%q", from, to)
	}
	if !CanTransition(from, to) {
		return fmt.Errorf("turn: illegal transition %q→%q", from, to)
	}
	return nil
}

// AllPhases returns every phase in declaration order. Useful for tests
// and for iterating the full lifecycle in observability code.
func AllPhases() []Phase {
	return []Phase{
		PhasePending,
		PhaseLoading,
		PhaseRunning,
		PhaseWaitingApproval,
		PhasePersisting,
		PhaseDone,
		PhaseErrored,
		PhaseInterrupted,
	}
}
