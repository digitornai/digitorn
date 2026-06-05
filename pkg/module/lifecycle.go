package module

import (
	"fmt"
	"sync"
)

type State int

const (
	StateLoaded State = iota
	StateStarting
	StateActive
	StatePaused
	StateStopping
	StateDisabled
	StateError
)

func (s State) String() string {
	switch s {
	case StateLoaded:
		return "LOADED"
	case StateStarting:
		return "STARTING"
	case StateActive:
		return "ACTIVE"
	case StatePaused:
		return "PAUSED"
	case StateStopping:
		return "STOPPING"
	case StateDisabled:
		return "DISABLED"
	case StateError:
		return "ERROR"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", int(s))
	}
}

func (s State) CanTransition(next State) bool {
	switch s {
	case StateLoaded:
		return next == StateStarting || next == StateError
	case StateStarting:
		return next == StateActive || next == StateError
	case StateActive:
		return next == StatePaused || next == StateStopping || next == StateError
	case StatePaused:
		return next == StateActive || next == StateStopping || next == StateError
	case StateStopping:
		return next == StateDisabled || next == StateError
	case StateDisabled:
		return next == StateStarting
	case StateError:
		return next == StateStopping || next == StateDisabled
	default:
		return false
	}
}

type stateTracker struct {
	mu sync.RWMutex
	s  State
}

func (t *stateTracker) get() State {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.s
}

func (t *stateTracker) transition(next State) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.s.CanTransition(next) {
		return fmt.Errorf("invalid transition %s -> %s", t.s, next)
	}
	t.s = next
	return nil
}
