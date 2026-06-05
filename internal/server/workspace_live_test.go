package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/ports"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

func newTestLive(rt ports.RealtimeServer, window time.Duration,
	changes func(context.Context, string) ([]sessionstore.WorkspaceChangedFile, error)) *workspaceLive {
	return &workspaceLive{
		rt:      rt,
		builder: sessionstore.NewEnvelopeBuilder("inst-test", nil),
		changes: changes,
		log:     slog.Default(),
		window:  window,
		pend:    make(map[string]*wsPend),
	}
}

func (l *workspaceLive) pendLen() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.pend)
}

func emitCount(f *fakeRealtime) int { return len(f.recordedEmits()) }

// TestWorkspaceLive_CoalescesBurst : a storm of writes within the debounce
// window collapses to exactly ONE git status and ONE push carrying the changes.
func TestWorkspaceLive_CoalescesBurst(t *testing.T) {
	rt := newFakeRealtime()
	var calls int64
	l := newTestLive(rt, 50*time.Millisecond, func(_ context.Context, _ string) ([]sessionstore.WorkspaceChangedFile, error) {
		atomic.AddInt64(&calls, 1)
		return []sessionstore.WorkspaceChangedFile{{Path: "a.txt", Status: "added"}}, nil
	})

	for i := 0; i < 100; i++ {
		l.FileChanged("root", "/wd")
	}
	if c := atomic.LoadInt64(&calls); c != 0 {
		t.Fatalf("refresh must wait for the quiet window, ran %d times early", c)
	}
	waitUntil(t, func() bool { return emitCount(rt) == 1 }, "one coalesced emit")
	if c := atomic.LoadInt64(&calls); c != 1 {
		t.Fatalf("100 writes must coalesce to 1 refresh, got %d", c)
	}

	msg := rt.recordedEmits()[0]
	if msg.Room != "session:root" || msg.Event != "event" || msg.Namespace != bridgeNamespace {
		t.Fatalf("bad routing: %+v", msg)
	}
	env, ok := msg.Data.(sessionstore.SocketEnvelope)
	if !ok {
		t.Fatalf("emit data is not a SocketEnvelope: %T", msg.Data)
	}
	if env.Type != string(sessionstore.EventWorkspaceChanges) {
		t.Fatalf("envelope type = %q", env.Type)
	}
	pl, ok := env.Payload.(*sessionstore.WorkspaceChangesPayload)
	if !ok || pl.Count != 1 || len(pl.Files) != 1 || pl.Files[0].Path != "a.txt" {
		t.Fatalf("bad payload: %#v", env.Payload)
	}
	waitUntil(t, func() bool { return l.pendLen() == 0 }, "pend map cleaned after fire")
}

// TestWorkspaceLive_ReArmsOnWriteDuringRefresh : a write that lands WHILE a
// refresh is running must trigger a second refresh — never get dropped.
func TestWorkspaceLive_ReArmsOnWriteDuringRefresh(t *testing.T) {
	rt := newFakeRealtime()
	release := make(chan struct{})
	var calls int64
	l := newTestLive(rt, 20*time.Millisecond, func(_ context.Context, _ string) ([]sessionstore.WorkspaceChangedFile, error) {
		if atomic.AddInt64(&calls, 1) == 1 {
			<-release // hold the first refresh open so a write can race it
		}
		return nil, nil
	})

	l.FileChanged("root", "/wd")
	waitUntil(t, func() bool { return atomic.LoadInt64(&calls) == 1 }, "first refresh running")
	l.FileChanged("root", "/wd2") // arrives mid-refresh
	close(release)
	waitUntil(t, func() bool { return atomic.LoadInt64(&calls) == 2 }, "re-armed second refresh")
	waitUntil(t, func() bool { return l.pendLen() == 0 }, "settles clean")
}

// TestWorkspaceLive_SubAgentFoldsToRoot : a sub-agent write pushes to the
// TOP-LEVEL session room (where the user is watching), not the isolated
// sub-session, and nested agents of a tree fold onto that same root.
func TestWorkspaceLive_SubAgentFoldsToRoot(t *testing.T) {
	rt := newFakeRealtime()
	l := newTestLive(rt, 20*time.Millisecond, func(context.Context, string) ([]sessionstore.WorkspaceChangedFile, error) {
		return nil, nil
	})
	l.FileChanged("root123::agent::run9", "/wd")
	l.FileChanged("root123::agent::run9::agent::run42", "/wd") // nested, same tree
	waitUntil(t, func() bool { return emitCount(rt) >= 1 }, "emitted")
	for _, m := range rt.recordedEmits() {
		if m.Room != "session:root123" {
			t.Fatalf("sub-agent push must target the root room, got %q", m.Room)
		}
	}
}

// TestWorkspaceLive_ConcurrentNoLeak : 1000 concurrent signals across 50
// sessions never race and always drain the pending map (run with -race).
func TestWorkspaceLive_ConcurrentNoLeak(t *testing.T) {
	rt := newFakeRealtime()
	l := newTestLive(rt, 10*time.Millisecond, func(context.Context, string) ([]sessionstore.WorkspaceChangedFile, error) {
		return nil, nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l.FileChanged(fmt.Sprintf("s%d", i%50), "/wd")
		}(i)
	}
	wg.Wait()
	waitUntil(t, func() bool { return l.pendLen() == 0 }, "all debouncers drained")
	if emitCount(rt) < 1 {
		t.Fatal("expected at least one emit across the burst")
	}
}

// TestWorkspaceLive_NilAndEmptySafe : a nil notifier and empty args are no-ops,
// never a panic and never an emit.
func TestWorkspaceLive_NilAndEmptySafe(t *testing.T) {
	var nilLive *workspaceLive
	nilLive.FileChanged("s", "/wd") // must not panic

	rt := newFakeRealtime()
	l := newTestLive(rt, 10*time.Millisecond, func(context.Context, string) ([]sessionstore.WorkspaceChangedFile, error) {
		return nil, nil
	})
	l.FileChanged("", "/wd")
	l.FileChanged("s", "")
	time.Sleep(40 * time.Millisecond)
	if emitCount(rt) != 0 {
		t.Fatalf("empty args must not emit, got %d", emitCount(rt))
	}
	if l.pendLen() != 0 {
		t.Fatalf("empty args must not register pend state, got %d", l.pendLen())
	}
}

// TestWorkspaceLive_ChangesErrorNoEmit : a failed git status pushes nothing and
// still cleans up — a broken repo can never leak a debouncer.
func TestWorkspaceLive_ChangesErrorNoEmit(t *testing.T) {
	rt := newFakeRealtime()
	l := newTestLive(rt, 10*time.Millisecond, func(context.Context, string) ([]sessionstore.WorkspaceChangedFile, error) {
		return nil, errors.New("boom")
	})
	l.FileChanged("root", "/wd")
	waitUntil(t, func() bool { return l.pendLen() == 0 }, "cleaned after error")
	if emitCount(rt) != 0 {
		t.Fatalf("errored refresh must not emit, got %d", emitCount(rt))
	}
}
