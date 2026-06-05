package runtime

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestKeepaliveTicker_PingsUntilStop proves the ticker keeps the idle watchdog
// alive for the whole duration of a legitimate blocking wait (a long tool, or a
// pending human approval) and stops cleanly. This is the mechanism that stops a
// human taking minutes to approve from being read as a stalled turn and killed
// with "context canceled".
func TestKeepaliveTicker_PingsUntilStop(t *testing.T) {
	old := keepaliveTickInterval
	keepaliveTickInterval = 2 * time.Millisecond
	defer func() { keepaliveTickInterval = old }()

	var pings int64
	ctx := WithTurnKeepalive(context.Background(), func() { atomic.AddInt64(&pings, 1) })
	stop := make(chan struct{})
	go keepaliveTicker(ctx, stop)

	time.Sleep(40 * time.Millisecond) // ~20 ticks worth
	close(stop)
	if got := atomic.LoadInt64(&pings); got < 3 {
		t.Fatalf("expected the ticker to ping the watchdog repeatedly during the wait, got %d", got)
	}
	// After stop the ticker exits ; at most one already-scheduled async ping may
	// still land, then the count must be STABLE (no leak past the guarded wait).
	time.Sleep(20 * time.Millisecond)
	settled := atomic.LoadInt64(&pings)
	time.Sleep(30 * time.Millisecond) // ~15 tick intervals — would grow if still running
	if final := atomic.LoadInt64(&pings); final != settled {
		t.Fatalf("ticker kept pinging after stop: %d -> %d", settled, final)
	}
}

// TestKeepalive_PingInvokesAttachedCallback : a ping fires the attached
// progress callback exactly when one is present, and is a safe no-op otherwise.
func TestKeepalive_PingInvokesAttachedCallback(t *testing.T) {
	// No callback attached → no panic, nothing happens.
	PingTurnKeepalive(context.Background())

	var pings int
	ctx := WithTurnKeepalive(context.Background(), func() { pings++ })
	PingTurnKeepalive(ctx)
	PingTurnKeepalive(ctx)
	if pings != 2 {
		t.Fatalf("expected 2 pings, got %d", pings)
	}

	// A nil fn leaves the context untouched (ping stays a no-op).
	if got := WithTurnKeepalive(context.Background(), nil); got == nil {
		t.Fatal("WithTurnKeepalive(nil) returned a nil context")
	}
}

// TestIsLongRunningTool pins the exemption set : human-in-the-loop and sub-flow
// tools must be EXEMPT from the per-call timeout (bare name or dotted FQN);
// ordinary leaf tools must NOT be, so they stay bounded.
func TestIsLongRunningTool(t *testing.T) {
	exempt := []string{
		"ask_user", "context_builder.ask_user",
		"run_parallel", "use_skill", "call_app",
		"agent_spawn.spawn", "agent_spawn.delegate",
	}
	for _, n := range exempt {
		if !isLongRunningTool(n) {
			t.Errorf("%q should be exempt from the per-tool timeout", n)
		}
	}
	bounded := []string{
		"filesystem.read", "filesystem.grep", "bash.run",
		"execute_tool", "search_tools", "background_run",
		"database.query", "agent_spawnish.x",
	}
	for _, n := range bounded {
		if isLongRunningTool(n) {
			t.Errorf("%q must stay bounded by the per-tool timeout", n)
		}
	}
}
