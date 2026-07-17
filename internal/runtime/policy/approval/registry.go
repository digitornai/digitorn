package approval

import (
	"context"
	"sync"
	"time"
)

type Result int

const (
	ResultApproved Result = iota

	ResultDenied

	ResultTimeout

	ResultCancelled

	ResultApprovedAlways
)

func (r Result) String() string {
	switch r {
	case ResultApproved:
		return "approved"
	case ResultApprovedAlways:
		return "approved_always"
	case ResultDenied:
		return "denied"
	case ResultTimeout:
		return "timeout"
	case ResultCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}

type Resolution struct {
	Result Result
	Reason string
}

type Registry struct {
	mu      sync.Mutex
	waiters map[string]chan Resolution
}

func NewRegistry() *Registry {
	return &Registry{
		waiters: make(map[string]chan Resolution),
	}
}

func (r *Registry) Wait(ctx context.Context, requestID string, timeout time.Duration) Resolution {
	return r.Arm(requestID).Wait(ctx, timeout)
}

func (r *Registry) Arm(requestID string) *Pending {
	if r == nil {
		return &Pending{}
	}
	return &Pending{r: r, id: requestID, ch: r.register(requestID)}
}

type Pending struct {
	r  *Registry
	id string
	ch chan Resolution
}

func (p *Pending) Wait(ctx context.Context, timeout time.Duration) Resolution {
	if p == nil || p.r == nil {
		return Resolution{Result: ResultCancelled, Reason: "no approval registry wired"}
	}
	defer p.r.unregister(p.id)

	var timeoutCh <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timeoutCh = t.C
	}

	select {
	case res := <-p.ch:
		return res
	case <-ctx.Done():
		return Resolution{Result: ResultCancelled, Reason: ctx.Err().Error()}
	case <-timeoutCh:
		return Resolution{Result: ResultTimeout,
			Reason: "approval not granted within " + timeout.String()}
	}
}

func (r *Registry) Resolve(requestID string, res Resolution) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	ch, ok := r.waiters[requestID]
	if !ok {
		r.mu.Unlock()
		return false
	}
	delete(r.waiters, requestID)
	r.mu.Unlock()
	ch <- res
	return true
}

func (r *Registry) register(requestID string) chan Resolution {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch := make(chan Resolution, 1)
	r.waiters[requestID] = ch
	return ch
}

func (r *Registry) unregister(requestID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.waiters, requestID)
}

func (r *Registry) Pending() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.waiters)
}
