package indexer

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// flapConn is a Watch connector whose behaviour is scripted : it records the
// wall-clock instant of every Watch() invocation (so the test can read the
// supervisor's restart spacing), then either errors immediately (flap) or
// blocks until released (a healthy run), per the controller.
type flapConn struct {
	mu       sync.Mutex
	starts   []time.Time
	healthy  atomic.Bool // when true, Watch blocks (simulates a long healthy run)
	released chan struct{}
}

func (f *flapConn) Type() string                                                 { return "flap" }
func (f *flapConn) Capabilities() Caps                                           { return Caps{Watch: true} }
func (f *flapConn) Walk(context.Context, SourceSpec, func(Document) error) error { return nil }
func (f *flapConn) Watch(ctx context.Context, _ SourceSpec, _ Sink, _ Cursor) error {
	f.mu.Lock()
	f.starts = append(f.starts, time.Now())
	f.mu.Unlock()
	if f.healthy.Load() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.released:
			return context.Canceled
		}
	}
	return context.DeadlineExceeded // flap : error immediately
}

func (f *flapConn) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.starts)
}

// gap returns the spacing between Watch start i-1 and i (0 if out of range).
func (f *flapConn) gap(i int) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i <= 0 || i >= len(f.starts) {
		return 0
	}
	return f.starts[i].Sub(f.starts[i-1])
}

func (f *flapConn) gaps() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	var g []time.Duration
	for i := 1; i < len(f.starts); i++ {
		g = append(g, f.starts[i].Sub(f.starts[i-1]))
	}
	return g
}

// waitCount blocks until at least n Watch starts have been recorded, or fails.
func (f *flapConn) waitCount(t *testing.T, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for f.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d Watch starts after %v, wanted %d", f.count(), within, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestWatchBackoff_ResetsAfterHealthyRun proves the supervisor escalates the
// restart backoff while a stream flaps (1s→2s→4s…), but RESETS it to the floor
// after the stream runs healthy past watchReset — so post-recovery latency does
// not stay stuck at the escalated delay. Regression for the escalate-only
// defect found by the indexer chaos probe.
func TestWatchBackoff_ResetsAfterHealthyRun(t *testing.T) {
	fc := &flapConn{released: make(chan struct{})}
	Register(fc)
	svc := NewService(NewMemCursor(), 2)
	svc.watchReset = 250 * time.Millisecond // a short healthy run counts as healthy in-test
	spec := SourceSpec{Name: "flap", Type: "flap", KB: "kb", Triggers: []Trigger{{Type: "watch"}}}
	svc.Register(spec, newRecordingSink())
	defer svc.Shutdown(context.Background())

	// Phase 1 — flap : let it escalate for ~7.5s ⇒ gaps ≈ [1s, 2s, 4s].
	time.Sleep(7500 * time.Millisecond)
	gaps := fc.gaps()
	t.Logf("escalation gaps: %v", gaps)
	if len(gaps) < 3 {
		t.Fatalf("expected ≥3 restart gaps in 7.5s, got %d (%v)", len(gaps), gaps)
	}
	if !(gaps[1] > gaps[0] && gaps[2] > gaps[1]) {
		t.Fatalf("backoff did not escalate while flapping: %v", gaps)
	}

	// Phase 2 — heal : the next Watch blocks (a healthy run) for ~1s, well past
	// the 250ms reset threshold.
	healIdx := fc.count() // index of the upcoming healthy (blocking) Watch start
	fc.healthy.Store(true)
	fc.waitCount(t, healIdx+1, 12*time.Second) // wait until the healthy Watch has started
	time.Sleep(1 * time.Second)                // hold the healthy run > watchReset

	// Phase 3 — recover : drop the healthy stream and let it flap again.
	fc.healthy.Store(false)
	close(fc.released)
	fc.waitCount(t, healIdx+2, 12*time.Second) // wait for the first post-healthy restart

	// The restart that follows the healthy run must come at the RESET floor
	// (healthy ~1s + ~1s backoff ≈ 2s), NOT the escalated delay (~1s + 8s ≈ 9s).
	postHealthyGap := fc.gap(healIdx + 1)
	t.Logf("post-healthy restart gap = %v (escalated would be ~9s; reset floor ~2s)", postHealthyGap)
	if postHealthyGap >= 5*time.Second {
		t.Fatalf("backoff did NOT reset after a healthy run : gap %v (recovery latency stuck at escalated delay)", postHealthyGap)
	}
}
