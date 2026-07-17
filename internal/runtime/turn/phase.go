package turn

import "fmt"

type Phase string

const (
	PhasePending Phase = "pending"

	PhaseLoading Phase = "loading"

	PhaseRunning Phase = "running"

	PhaseWaitingApproval Phase = "waiting_approval"

	PhasePersisting Phase = "persisting"

	PhaseDone Phase = "done"

	PhaseErrored Phase = "errored"

	PhaseInterrupted Phase = "interrupted"
)

func (p Phase) IsTerminal() bool {
	switch p {
	case PhaseDone, PhaseErrored, PhaseInterrupted:
		return true
	}
	return false
}

func (p Phase) IsInFlight() bool {
	switch p {
	case PhaseLoading, PhaseRunning, PhaseWaitingApproval, PhasePersisting:
		return true
	}
	return false
}

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

func CanTransition(from, to Phase) bool {
	allowed, ok := validTransitions[from]
	if !ok {
		return false
	}
	return allowed[to]
}

func Validate(from, to Phase) error {
	if from == to {
		return fmt.Errorf("turn: no-op transition %q→%q", from, to)
	}
	if !CanTransition(from, to) {
		return fmt.Errorf("turn: illegal transition %q→%q", from, to)
	}
	return nil
}

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
