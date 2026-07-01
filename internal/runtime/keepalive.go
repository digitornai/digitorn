package runtime

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/digitornai/digitorn/internal/safego"
)

// keepaliveTickInterval is how often a long-running / human-in-the-loop tool
// (or a pending human approval) pings the watchdog while it waits. Comfortably
// below any sane idle window so the turn is never judged stalled mid-legitimate-
// wait. A var (not const) only so tests can shrink it; production never mutates it.
var keepaliveTickInterval = 30 * time.Second

// keepaliveTicker pings the turn watchdog every keepaliveTickInterval until
// stop is closed or ctx ends. Used while a long/human tool (ask_user, a
// sub-flow) is dispatched, so its legitimate long duration doesn't trip the
// idle cutoff. Exits promptly; never leaks past the dispatch it guards.
func keepaliveTicker(ctx context.Context, stop <-chan struct{}) {
	t := time.NewTicker(keepaliveTickInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			safego.Run("runtime.keepalive", func() { PingTurnKeepalive(ctx) })
		}
	}
}

// isLongRunningTool reports whether a tool legitimately runs long or blocks on
// a human, and so must be EXEMPT from the per-call timeout (it has its own
// bound). Matches on the action segment so it works for bare meta names
// ("ask_user") and dotted FQNs ("context_builder.ask_user") alike, plus the
// agent_spawn delegation module (which runs a whole sub-agent turn).
func isLongRunningTool(name string) bool {
	if strings.HasPrefix(name, "agent_spawn.") {
		return true
	}
	action := name
	if i := strings.LastIndexByte(action, '.'); i >= 0 {
		action = action[i+1:]
	}
	switch action {
	case "ask_user", "run_parallel", "use_skill", "call_app":
		return true
	}
	return false
}

// ErrTurnSafetyCutoff is the cause set when the session runner's idle watchdog
// kills a turn that stopped making progress. It lives here (not in the server
// package) so the engine can recognise it via context.Cause and end the turn
// with a clear, human-readable note instead of an anonymous "context canceled".
var ErrTurnSafetyCutoff = errors.New("turn safety cutoff exceeded (no progress)")

// turnKeepaliveKey carries a per-turn "still making progress" callback the
// engine invokes whenever the turn advances — an LLM round completes, a tool
// batch finishes. The session runner's safety watchdog resets its idle timer on
// each call, so a turn that keeps doing real work is never killed; only a turn
// that genuinely STALLS (no progress for the whole idle window) trips the
// cutoff. This is what lets a long-but-productive turn (a slow grep, a build,
// many tool rounds) run as long as it needs without a fixed wall-clock ceiling.
type turnKeepaliveKey struct{}

// WithTurnKeepalive attaches the progress callback. The session runner sets it
// before invoking the engine; the engine pings it at each loop step. A nil fn
// leaves the context unchanged (keepalive becomes a no-op — safe for tests and
// any caller that runs the engine without a watchdog).
func WithTurnKeepalive(ctx context.Context, fn func()) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, turnKeepaliveKey{}, fn)
}

// PingTurnKeepalive signals turn progress to the watchdog, if one is attached.
// Exported so an embedder driving the engine directly (or a test) can mark
// progress; the engine calls it at each loop step.
func PingTurnKeepalive(ctx context.Context) {
	if fn, ok := ctx.Value(turnKeepaliveKey{}).(func()); ok && fn != nil {
		fn()
	}
}
