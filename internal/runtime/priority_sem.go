package runtime

import (
	"context"
	"sync"
)

type Priority int

const (
	PriorityLow      Priority = 0
	PriorityNormal   Priority = 1
	PriorityHigh     Priority = 2
	PriorityCritical Priority = 3
)

type semWaiter struct {
	ch       chan struct{}
	priority Priority
}

type PrioritySemaphore struct {
	mu      sync.Mutex
	slots   int
	used    int
	waiters [4][]semWaiter
}

func NewPrioritySemaphore(slots int) *PrioritySemaphore {
	return &PrioritySemaphore{slots: slots}
}

func (s *PrioritySemaphore) Acquire(ctx context.Context, p Priority) error {
	s.mu.Lock()
	if s.used < s.slots {
		s.used++
		s.mu.Unlock()
		return nil
	}
	w := semWaiter{ch: make(chan struct{}, 1), priority: p}
	s.waiters[p] = append(s.waiters[p], w)
	s.mu.Unlock()

	select {
	case <-w.ch:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		q := s.waiters[p]
		for i, v := range q {
			if v.ch == w.ch {
				s.waiters[p] = append(q[:i], q[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
		return ctx.Err()
	}
}

func (s *PrioritySemaphore) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for p := PriorityCritical; p >= PriorityLow; p-- {
		if len(s.waiters[p]) > 0 {
			w := s.waiters[p][0]
			s.waiters[p] = s.waiters[p][1:]
			w.ch <- struct{}{}
			return
		}
	}
	s.used--
}
