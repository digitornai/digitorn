package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/runtime"
)

// SubmitUserTurn owns WHEN a user message is written. The whole point of the
// queue is that a message sent mid-turn does NOT enter the transcript until its
// turn starts — so the persist callback fires at exactly the right moment.

func TestSubmit_IdlePersistsImmediatelyAndRuns(t *testing.T) {
	ran := make(chan struct{}, 1)
	r := newTestRunner(func(_ context.Context, _ runtime.TurnInput) error {
		ran <- struct{}{}
		return nil
	})

	var persisted int32
	var mu sync.Mutex
	persist := func() (uint64, error) {
		mu.Lock()
		persisted++
		mu.Unlock()
		return 42, nil
	}

	queued, _, seq, err := r.SubmitUserTurn(
		runtime.TurnInput{AppID: "a", SessionID: "s1", UserID: "u"}, persist)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if queued {
		t.Fatal("idle session must run immediately, not queue")
	}
	if seq != 42 {
		t.Errorf("seq = %d, want 42 (from persist)", seq)
	}
	mu.Lock()
	if persisted != 1 {
		t.Errorf("persist called %d times, want 1 (now)", persisted)
	}
	mu.Unlock()
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("turn never ran")
	}
}

func TestSubmit_QueuedDefersPersistUntilItsTurn(t *testing.T) {
	release := make(chan struct{})
	firstRunning := make(chan struct{})
	var once sync.Once
	r := newTestRunner(func(_ context.Context, in runtime.TurnInput) error {
		if in.Skill == "first" {
			once.Do(func() { close(firstRunning) })
			<-release
		}
		return nil
	})

	// First message: idle → runs, holds the lane open.
	_, _, _, _ = r.SubmitUserTurn(
		runtime.TurnInput{AppID: "a", SessionID: "s1", Skill: "first"},
		func() (uint64, error) { return 1, nil })
	<-firstRunning

	// Second message arrives mid-turn. Its persist MUST NOT fire yet.
	var secondPersisted int32
	var mu sync.Mutex
	queued, depth, _, err := r.SubmitUserTurn(
		runtime.TurnInput{AppID: "a", SessionID: "s1", Skill: "second"},
		func() (uint64, error) {
			mu.Lock()
			secondPersisted++
			mu.Unlock()
			return 2, nil
		})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !queued || depth != 1 {
		t.Fatalf("second message: queued=%v depth=%d, want true/1", queued, depth)
	}
	mu.Lock()
	early := secondPersisted
	mu.Unlock()
	if early != 0 {
		t.Fatalf("queued message was persisted early (%d) — it would show in the chat before its turn", early)
	}

	// Let the first turn finish: the loop dequeues #2 and persists it NOW.
	close(release)
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		done := secondPersisted == 1
		mu.Unlock()
		if done {
			break
		}
		select {
		case <-deadline:
			t.Fatal("queued message was never persisted at dequeue")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestSubmit_PersistFailureIdleReleasesLane(t *testing.T) {
	r := newTestRunner(func(_ context.Context, _ runtime.TurnInput) error { return nil })

	queued, _, _, err := r.SubmitUserTurn(
		runtime.TurnInput{AppID: "a", SessionID: "s1"},
		func() (uint64, error) { return 0, errors.New("disk full") })
	if err == nil {
		t.Fatal("expected the persist error to surface")
	}
	if queued {
		t.Fatal("a failed idle submit must not report queued")
	}
	// The lane must be free again: a fresh submit runs.
	ran := make(chan struct{}, 1)
	r2 := r // same runner
	_ = r2
	r.exec = func(_ context.Context, _ runtime.TurnInput) error {
		ran <- struct{}{}
		return nil
	}
	_, _, _, err = r.SubmitUserTurn(
		runtime.TurnInput{AppID: "a", SessionID: "s1"},
		func() (uint64, error) { return 7, nil })
	if err != nil {
		t.Fatalf("second submit err: %v", err)
	}
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("lane stayed locked after a persist failure")
	}
}

// Two queued messages persist in FIFO order, each at its own dequeue.
func TestSubmit_QueuedPersistOrderIsFIFO(t *testing.T) {
	release := make(chan struct{})
	firstRunning := make(chan struct{})
	var once sync.Once
	r := newTestRunner(func(_ context.Context, in runtime.TurnInput) error {
		if in.Skill == "first" {
			once.Do(func() { close(firstRunning) })
			<-release
		}
		return nil
	})

	var mu sync.Mutex
	var order []string
	mkPersist := func(tag string) func() (uint64, error) {
		return func() (uint64, error) {
			mu.Lock()
			order = append(order, tag)
			mu.Unlock()
			return 1, nil
		}
	}

	_, _, _, _ = r.SubmitUserTurn(
		runtime.TurnInput{AppID: "a", SessionID: "s1", Skill: "first"}, mkPersist("first"))
	<-firstRunning
	r.SubmitUserTurn(runtime.TurnInput{AppID: "a", SessionID: "s1", Skill: "b"}, mkPersist("b"))
	r.SubmitUserTurn(runtime.TurnInput{AppID: "a", SessionID: "s1", Skill: "c"}, mkPersist("c"))
	close(release)

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		n := len(order)
		mu.Unlock()
		if n == 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("persists incomplete: %v", order)
		case <-time.After(5 * time.Millisecond):
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"first", "b", "c"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("persist order = %v, want %v", order, want)
		}
	}
}

// Cancelling a WAITING message removes it so its turn never runs.
func TestSubmit_CancelQueuedRemovesWaitingMessage(t *testing.T) {
	var mu sync.Mutex
	var ran []string
	release := make(chan struct{})
	firstRunning := make(chan struct{})
	var once sync.Once
	r := newTestRunner(func(_ context.Context, in runtime.TurnInput) error {
		mu.Lock()
		ran = append(ran, in.ClientMessageID)
		mu.Unlock()
		if in.ClientMessageID == "first" {
			once.Do(func() { close(firstRunning) })
			<-release
		}
		return nil
	})
	P := func() (uint64, error) { return 1, nil }

	r.SubmitUserTurn(runtime.TurnInput{AppID: "a", SessionID: "s1", ClientMessageID: "first"}, P)
	<-firstRunning
	r.SubmitUserTurn(runtime.TurnInput{AppID: "a", SessionID: "s1", ClientMessageID: "a"}, P)
	r.SubmitUserTurn(runtime.TurnInput{AppID: "a", SessionID: "s1", ClientMessageID: "b"}, P)

	if !r.CancelQueued("s1", "a") {
		t.Fatal("CancelQueued(a) returned false, want true")
	}
	if r.CancelQueued("s1", "ghost") {
		t.Fatal("CancelQueued(ghost) returned true for a non-existent id")
	}

	close(release)
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		n := len(ran)
		mu.Unlock()
		if n == 2 { // first + b, never a
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			t.Fatalf("ran = %v, want [first b]", ran)
			mu.Unlock()
		case <-time.After(5 * time.Millisecond):
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, id := range ran {
		if id == "a" {
			t.Fatalf("cancelled message 'a' ran anyway: %v", ran)
		}
	}
}

// Clearing drops every waiting message and returns their ids.
func TestSubmit_ClearQueuedDropsAllWaiting(t *testing.T) {
	var mu sync.Mutex
	var ran []string
	release := make(chan struct{})
	firstRunning := make(chan struct{})
	var once sync.Once
	r := newTestRunner(func(_ context.Context, in runtime.TurnInput) error {
		mu.Lock()
		ran = append(ran, in.ClientMessageID)
		mu.Unlock()
		if in.ClientMessageID == "first" {
			once.Do(func() { close(firstRunning) })
			<-release
		}
		return nil
	})
	P := func() (uint64, error) { return 1, nil }

	r.SubmitUserTurn(runtime.TurnInput{AppID: "a", SessionID: "s1", ClientMessageID: "first"}, P)
	<-firstRunning
	r.SubmitUserTurn(runtime.TurnInput{AppID: "a", SessionID: "s1", ClientMessageID: "a"}, P)
	r.SubmitUserTurn(runtime.TurnInput{AppID: "a", SessionID: "s1", ClientMessageID: "b"}, P)

	ids := r.ClearQueued("s1")
	if len(ids) != 2 {
		t.Fatalf("ClearQueued returned %v, want [a b]", ids)
	}

	close(release)
	// Give the loop a moment; only "first" should ever run.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			goto check
		case <-time.After(20 * time.Millisecond):
			mu.Lock()
			n := len(ran)
			mu.Unlock()
			if n > 1 {
				goto check
			}
		}
	}
check:
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 1 || ran[0] != "first" {
		t.Fatalf("ran = %v, want [first] (a, b cleared)", ran)
	}
}
