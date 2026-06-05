// Package approval implements the synchronous-pause registry that
// backs SG-5 (docs-site/docs/tutorial/security-01-approval.md). When
// gate 4 resolves a tool_call to "approve", the runtime goroutine
// registers a waiter here and blocks until a human resolution arrives
// (via the HTTP/Socket.IO surface) or the timeout fires.
//
// Process-local by design : the doc describes a synchronous pause of
// the agent loop, not a durable suspension that survives daemon
// restart. A crash mid-approval drops the waiter ; the next boot
// reconstructs the pending approval from the durable
// EventApprovalRequest event but doesn't auto-resume the original
// goroutine (the client must re-issue the chat turn).
//
// Concurrency : the registry is safe for concurrent Register /
// Resolve calls. A waiter may be resolved at most once ; subsequent
// resolutions are no-ops.
package approval

import (
	"context"
	"sync"
	"time"
)

// Result is the outcome of a single approval wait.
type Result int

const (
	// ResultApproved means a human (or programmatic supervisor)
	// resolved the request positively. Dispatcher proceeds.
	ResultApproved Result = iota

	// ResultDenied means an explicit denial. Tool result becomes
	// "permission_denied" and the agent loop resumes.
	ResultDenied

	// ResultTimeout means the configured approval_timeout elapsed
	// without resolution. Doc default 300s, range [30, 3600].
	// Equivalent to deny for the agent.
	ResultTimeout

	// ResultCancelled means the caller's context was cancelled
	// (turn interrupted, daemon shutdown). The waiter is removed
	// without resolution.
	ResultCancelled
)

// String renders the result for audit rows and logs.
func (r Result) String() string {
	switch r {
	case ResultApproved:
		return "approved"
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

// Resolution is what flows through the per-waiter channel : the
// outcome + an optional human-readable reason (for the audit row
// and the synthetic tool_result).
type Resolution struct {
	Result Result
	Reason string
}

// Registry holds the in-flight approvals. Build one per daemon (or
// per test). nil registry = no enforcement (SG-5 disabled at
// runtime).
type Registry struct {
	mu      sync.Mutex
	waiters map[string]chan Resolution
}

// NewRegistry constructs an empty registry. Cheap : no goroutines,
// no background workers. Cleanup happens lazily as Wait returns and
// removes the entry.
func NewRegistry() *Registry {
	return &Registry{
		waiters: make(map[string]chan Resolution),
	}
}

// Wait blocks until the approval identified by requestID is
// resolved, the timeout fires, or ctx is cancelled. timeout=0 means
// "no timeout" (wait forever until ctx is done or resolution
// arrives) ; production code should always pass the documented
// default (300s) or the app's configured value.
//
// Returns the Resolution describing what happened. The waiter is
// removed from the registry before Wait returns ; subsequent
// Resolve calls for the same requestID become no-ops.
//
// Safe to call concurrently for different requestIDs. Calling Wait
// twice for the same requestID is a programmer bug (only one waiter
// per id is allowed) — the second call shadows the first, leaving
// the first hung until ctx fires.
func (r *Registry) Wait(ctx context.Context, requestID string, timeout time.Duration) Resolution {
	return r.Arm(requestID).Wait(ctx, timeout)
}

// Arm registers a waiter for requestID and returns a Pending handle WITHOUT
// blocking. Call Arm BEFORE emitting the request event a resolver can observe,
// then call Pending.Wait to block. Arming first guarantees a fast Resolve can
// never beat the waiter (the emit-before-wait race), so the registry needs no
// speculative buffer and an unknown Resolve stays a clean no-op (no backlog, no
// leak). nil registry → a Pending that resolves to "no registry wired".
func (r *Registry) Arm(requestID string) *Pending {
	if r == nil {
		return &Pending{}
	}
	return &Pending{r: r, id: requestID, ch: r.register(requestID)}
}

// Pending is an armed waiter returned by Arm. Wait blocks on it exactly once.
type Pending struct {
	r  *Registry
	id string
	ch chan Resolution
}

// Wait blocks until the armed approval is resolved, the timeout fires (0 = no
// timeout), or ctx is cancelled. The waiter is removed before Wait returns, so
// later Resolve calls for the same id are no-ops.
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

// Resolve signals the waiter for requestID. Returns true when a
// waiter was found and signaled ; false when no waiter is
// registered (the request_id is unknown, already resolved, or
// already timed out).
//
// Safe to call concurrently with Wait and with itself. The
// underlying channel is buffered with size 1 so the send never
// blocks even if the waiter has already left.
func (r *Registry) Resolve(requestID string, res Resolution) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	ch, ok := r.waiters[requestID]
	if !ok {
		r.mu.Unlock()
		return false // unknown / already resolved / already timed out : no-op
	}
	// Claim the waiter under the lock : delete it so a concurrent or repeated
	// Resolve for the same id finds nothing and returns false. The channel is
	// buffered (cap 1) and freshly claimed, so the send below never blocks.
	delete(r.waiters, requestID)
	r.mu.Unlock()
	ch <- res
	return true
}

// register creates the per-waiter channel under the lock and
// returns it. Buffered size 1 so Resolve can never block.
func (r *Registry) register(requestID string) chan Resolution {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch := make(chan Resolution, 1)
	r.waiters[requestID] = ch
	return ch
}

// unregister removes the waiter after Wait returns. Always called
// via defer in Wait — guarantees no leak even on panic.
func (r *Registry) unregister(requestID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.waiters, requestID)
}

// Pending returns the number of in-flight waiters. Useful for
// observability (a session with many pending approvals is stuck on
// a human) and for tests that need to wait for the registry to
// reach a known state.
func (r *Registry) Pending() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.waiters)
}
