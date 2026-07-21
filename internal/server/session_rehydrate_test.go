package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/runtime"
)

// After a daemon restart the durable queue still holds messages the in-memory
// runner lost. rehydrateQueue recovers them: text messages re-run in order, a
// message with attachments (blobs gone) is dropped, and it happens exactly once.

func attachRunner(d *Daemon, exec func(context.Context, runtime.TurnInput) error) *sessionRunner {
	r := newSessionRunner(exec, time.Minute, d.logger)
	r.queuedHook = d.onTurnQueued
	r.dequeuedHook = d.onTurnDequeued
	d.sessionRunner = r
	return r
}

// Simulate the post-restart state: the durable projection has queued rows, but
// the runner's in-memory queue does NOT (onTurnQueued only writes the event).
func seedOrphan(d *Daemon, sid, cid, msg string, attachments int) {
	d.onTurnQueued(runtime.TurnInput{
		AppID: "a", SessionID: sid, UserID: "u",
		ClientMessageID: cid, Message: msg, AttachmentCount: attachments,
	}, 1)
}

func waitRan(t *testing.T, mu *sync.Mutex, ran *[]string, n int, what string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		got := len(*ran)
		mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-deadline:
			mu.Lock()
			t.Fatalf("%s: ran=%v, want %d entries", what, *ran, n)
			mu.Unlock()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestRehydrate_ReExecutesOrphanTextInOrder(t *testing.T) {
	d, done := newQueueTestDaemon(t)
	defer done()
	var mu sync.Mutex
	var ran []string
	attachRunner(d, func(_ context.Context, in runtime.TurnInput) error {
		mu.Lock()
		ran = append(ran, in.ClientMessageID)
		mu.Unlock()
		return nil
	})

	seedOrphan(d, "s1", "c1", "un", 0)
	seedOrphan(d, "s1", "c2", "deux", 0)
	if q := queueOf(t, d, "s1"); len(q) != 2 {
		t.Fatalf("seed: projection has %d rows, want 2", len(q))
	}

	d.rehydrateQueue("s1", "a", "u", "jwt")

	waitRan(t, &mu, &ran, 2, "both orphans re-run")
	mu.Lock()
	defer mu.Unlock()
	if ran[0] != "c1" || ran[1] != "c2" {
		t.Fatalf("ran = %v, want [c1 c2] (FIFO)", ran)
	}
}

func TestRehydrate_IsOneShot(t *testing.T) {
	d, done := newQueueTestDaemon(t)
	defer done()
	var mu sync.Mutex
	var ran []string
	attachRunner(d, func(_ context.Context, in runtime.TurnInput) error {
		mu.Lock()
		ran = append(ran, in.ClientMessageID)
		mu.Unlock()
		return nil
	})

	seedOrphan(d, "s1", "c1", "un", 0)

	d.rehydrateQueue("s1", "a", "u", "jwt")
	d.rehydrateQueue("s1", "a", "u", "jwt") // second open: must be a no-op
	d.rehydrateQueue("s1", "a", "u", "jwt")

	waitRan(t, &mu, &ran, 1, "orphan re-run once")
	time.Sleep(100 * time.Millisecond) // give any erroneous re-run time to appear
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 1 {
		t.Fatalf("ran = %v, want exactly 1 (guard failed → double execution)", ran)
	}
}

func TestRehydrate_DropsOrphanWithAttachments(t *testing.T) {
	d, done := newQueueTestDaemon(t)
	defer done()
	var mu sync.Mutex
	var ran []string
	attachRunner(d, func(_ context.Context, in runtime.TurnInput) error {
		mu.Lock()
		ran = append(ran, in.ClientMessageID)
		mu.Unlock()
		return nil
	})

	seedOrphan(d, "s1", "c1", "avec image", 2) // blobs not recoverable
	seedOrphan(d, "s1", "c2", "texte", 0)

	d.rehydrateQueue("s1", "a", "u", "jwt")

	waitRan(t, &mu, &ran, 1, "text orphan re-run")
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 1 || ran[0] != "c2" {
		t.Fatalf("ran = %v, want [c2] only — the attachment message must be dropped, not sent stripped", ran)
	}
}

func TestRehydrate_SkipsWhenTurnRunning(t *testing.T) {
	d, done := newQueueTestDaemon(t)
	defer done()
	var mu sync.Mutex
	var ran []string
	release := make(chan struct{})
	running := make(chan struct{})
	var once sync.Once
	r := attachRunner(d, func(_ context.Context, in runtime.TurnInput) error {
		mu.Lock()
		ran = append(ran, in.ClientMessageID)
		mu.Unlock()
		if in.ClientMessageID == "live" {
			once.Do(func() { close(running) })
			<-release
		}
		return nil
	})

	// A real turn is in flight (registers inflight → IsRunning true).
	r.SubmitUserTurn(runtime.TurnInput{AppID: "a", SessionID: "s1", ClientMessageID: "live"},
		func() (uint64, error) { return 1, nil })
	<-running

	// A stale durable orphan exists, but the session is live: recovery must skip.
	seedOrphan(d, "s1", "c1", "orphan", 0)
	d.rehydrateQueue("s1", "a", "u", "jwt")

	time.Sleep(150 * time.Millisecond)
	close(release)
	mu.Lock()
	defer mu.Unlock()
	for _, id := range ran {
		if id == "c1" {
			t.Fatalf("recovery ran while a turn was live: %v", ran)
		}
	}
}
