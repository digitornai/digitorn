package server

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/ports"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// erroringRealtime records emits like fakeRealtime but every Emit FAILS, to
// prove the debouncer survives a dead transport.
type erroringRealtime struct{ *fakeRealtime }

func (e erroringRealtime) Emit(ctx context.Context, ns, room, ev string, data any) error {
	_ = e.fakeRealtime.Emit(ctx, ns, room, ev, data)
	return errors.New("transport down")
}

// TestWorkspaceLive_ScaleNoGoroutineLeak fires a debounce for 10k distinct
// sessions and proves the pending map fully drains and the goroutine count
// returns to baseline — no per-session goroutine, no leak (C1).
func TestWorkspaceLive_ScaleNoGoroutineLeak(t *testing.T) {
	rt := newFakeRealtime()
	var calls int64
	l := newTestLive(rt, 15*time.Millisecond, func(context.Context, string) ([]sessionstore.WorkspaceChangedFile, error) {
		atomic.AddInt64(&calls, 1)
		return nil, nil
	})

	runtime.GC()
	base := runtime.NumGoroutine()

	const n = 10000
	for i := 0; i < n; i++ {
		l.FileChanged(fmt.Sprintf("s%d", i), "/wd")
	}
	waitUntil(t, func() bool { return l.pendLen() == 0 }, "all 10k debouncers drained")
	if c := atomic.LoadInt64(&calls); c != n {
		t.Fatalf("each distinct session must refresh once: got %d want %d", c, n)
	}
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()
	if after > base+20 {
		t.Fatalf("goroutine leak: base=%d after=%d (delta %d)", base, after, after-base)
	}
}

// TestWorkspaceLive_EmitErrorResilient proves a failing transport never crashes
// the debouncer and the pending entry is still cleaned up.
func TestWorkspaceLive_EmitErrorResilient(t *testing.T) {
	rt := erroringRealtime{newFakeRealtime()}
	l := newTestLive(rt, 10*time.Millisecond, func(context.Context, string) ([]sessionstore.WorkspaceChangedFile, error) {
		return []sessionstore.WorkspaceChangedFile{{Path: "a.txt", Status: "added"}}, nil
	})
	l.FileChanged("root", "/wd")
	waitUntil(t, func() bool { return l.pendLen() == 0 }, "drained despite emit error")
	if emitCount(rt.fakeRealtime) < 1 {
		t.Fatal("emit was attempted (and failed) at least once")
	}
}

// TestWorkspaceLive_FileChangedNeverBlocks proves the hot-path signal returns
// immediately even while a refresh for the SAME session is in flight — the
// filesystem write that calls it is never slowed by git (E1 "jamais ralentir").
func TestWorkspaceLive_FileChangedNeverBlocks(t *testing.T) {
	rt := newFakeRealtime()
	release := make(chan struct{})
	var firstRunning int32
	l := newTestLive(rt, 10*time.Millisecond, func(context.Context, string) ([]sessionstore.WorkspaceChangedFile, error) {
		if atomic.AddInt32(&firstRunning, 1) == 1 {
			<-release // hold the refresh open
		}
		return nil, nil
	})

	l.FileChanged("root", "/wd")
	waitUntil(t, func() bool { return atomic.LoadInt32(&firstRunning) == 1 }, "refresh running")

	// FileChanged for the SAME session while its refresh is blocked must return
	// in microseconds (it only takes the mutex + sets a flag).
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		l.FileChanged("root", "/wd")
		done <- time.Since(start)
	}()
	select {
	case d := <-done:
		if d > 50*time.Millisecond {
			t.Fatalf("FileChanged blocked %s during an in-flight refresh (must be ~instant)", d)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("FileChanged BLOCKED on the in-flight git refresh — violates 'jamais ralentir'")
	}
	close(release)
}

// TestWorkspaceLive_HungRefreshDoesNotBlockOtherSessions proves one session's
// stuck git refresh can't stall another session's push — the fire goroutines are
// independent (E2 "jamais bloquer la loop").
func TestWorkspaceLive_HungRefreshDoesNotBlockOtherSessions(t *testing.T) {
	rt := newFakeRealtime()
	hang := make(chan struct{})
	l := newTestLive(rt, 10*time.Millisecond, func(_ context.Context, wd string) ([]sessionstore.WorkspaceChangedFile, error) {
		if wd == "/hung" {
			<-hang // session A's refresh never returns
		}
		return nil, nil
	})

	l.FileChanged("A", "/hung") // will hang in fire()
	l.FileChanged("B", "/ok")   // must still get pushed

	waitUntil(t, func() bool {
		for _, m := range rt.recordedEmits() {
			if m.Room == "session:B" {
				return true
			}
		}
		return false
	}, "session B pushed while A's refresh is hung")
	close(hang)
}

// TestWorkspaceLive_LevelTriggeredNeverLosesFinalState proves that after a burst,
// a push ALWAYS reflects the latest workspace state — the git-status model is
// level-triggered, so even if intermediate pushes coalesce away, the final state
// is never lost (E3 "jamais perdre").
func TestWorkspaceLive_LevelTriggeredNeverLosesFinalState(t *testing.T) {
	rt := newFakeRealtime()
	var state int64
	// changes() reflects the CURRENT state at refresh time (like a real git
	// status), stashing the observed value in the first path so the test reads it.
	l := newTestLive(rt, 8*time.Millisecond, func(context.Context, string) ([]sessionstore.WorkspaceChangedFile, error) {
		v := atomic.LoadInt64(&state)
		return []sessionstore.WorkspaceChangedFile{{Path: fmt.Sprintf("state=%d", v), Status: "modified"}}, nil
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			atomic.AddInt64(&state, 1)
			l.FileChanged("root", "/wd")
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()
	atomic.StoreInt64(&state, 999)
	l.FileChanged("root", "/wd") // the final signal

	want := "state=999"
	waitUntil(t, func() bool {
		ems := rt.recordedEmits()
		if len(ems) == 0 {
			return false
		}
		last := ems[len(ems)-1]
		env, ok := last.Data.(sessionstore.SocketEnvelope)
		if !ok {
			return false
		}
		pl, ok := env.Payload.(*sessionstore.WorkspaceChangesPayload)
		return ok && len(pl.Files) > 0 && pl.Files[0].Path == want
	}, "final state (999) is eventually pushed — never lost")
}

// TestWorkspaceLive_SustainedBurstStreamsUpdates proves a long continuous burst
// of writes (faster than the debounce window) streams INTERMEDIATE pushes via the
// max-wait cap — the web sees the latest state WHILE the agent works, not only
// once the burst ends. A pure trailing debounce would coalesce this to one push.
func TestWorkspaceLive_SustainedBurstStreamsUpdates(t *testing.T) {
	rt := newFakeRealtime()
	window := 20 * time.Millisecond // maxWait = 3*window = 60ms
	l := newTestLive(rt, window, func(context.Context, string) ([]sessionstore.WorkspaceChangedFile, error) {
		return nil, nil
	})

	stop := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(stop) {
		l.FileChanged("root", "/wd") // write every ~10ms, faster than the 20ms window
		time.Sleep(10 * time.Millisecond)
	}
	waitUntil(t, func() bool { return l.pendLen() == 0 }, "burst drained")

	n := 0
	for _, m := range rt.recordedEmits() {
		if m.Room == "session:root" {
			n++
		}
	}
	if n < 3 {
		t.Fatalf("a ~300ms sustained burst must stream several pushes via max-wait, got %d", n)
	}
}

var _ ports.RealtimeServer = erroringRealtime{}
