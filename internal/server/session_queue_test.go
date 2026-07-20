package server

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/runtime"
)

// The runner used to hold ONE pending turn (`st.next`), overwriting it on every
// arrival: three messages sent during a turn silently became one. These pin the
// FIFO and the atomicity of the "run now vs queue" decision.

func newTestRunner(exec func(ctx context.Context, in runtime.TurnInput) error) *sessionRunner {
	return &sessionRunner{
		exec:     exec,
		cutoff:   30 * time.Second,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		inflight: map[string]context.CancelCauseFunc{},
	}
}

func TestSessionRunner_QueuesEveryMessageInOrder(t *testing.T) {
	var mu sync.Mutex
	var ran []string
	release := make(chan struct{})
	first := make(chan struct{})
	var once sync.Once

	r := newTestRunner(func(_ context.Context, in runtime.TurnInput) error {
		mu.Lock()
		ran = append(ran, in.Skill)
		mu.Unlock()
		once.Do(func() { close(first) })
		<-release // hold the first turn open so the rest must queue
		return nil
	})

	base := runtime.TurnInput{AppID: "app", SessionID: "s1"}
	in := base
	in.Skill = "one"
	r.WakeTurn(in)
	<-first // the first turn is now executing

	for _, s := range []string{"two", "three", "four"} {
		q := base
		q.Skill = s
		r.WakeTurn(q)
	}
	close(release)

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		n := len(ran)
		mu.Unlock()
		if n == 4 {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			t.Fatalf("only %d turns ran, want 4: %v", len(ran), ran)
			mu.Unlock()
		case <-time.After(5 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"one", "two", "three", "four"}
	for i := range want {
		if ran[i] != want[i] {
			t.Fatalf("out of order: got %v, want %v", ran, want)
		}
	}
}

// The hooks fire once per queued turn, with a growing depth, and once per
// dequeue. A nil hook must stay a no-op (that is the "no behaviour change"
// guarantee for apps that never queue).
func TestSessionRunner_QueueHooksFireOncePerMessage(t *testing.T) {
	var mu sync.Mutex
	var depths []int
	dequeued := 0
	release := make(chan struct{})
	first := make(chan struct{})
	var once sync.Once

	r := newTestRunner(func(_ context.Context, _ runtime.TurnInput) error {
		once.Do(func() { close(first) })
		<-release
		return nil
	})
	r.queuedHook = func(_ runtime.TurnInput, depth int) {
		mu.Lock()
		depths = append(depths, depth)
		mu.Unlock()
	}
	r.dequeuedHook = func(_ runtime.TurnInput) {
		mu.Lock()
		dequeued++
		mu.Unlock()
	}

	in := runtime.TurnInput{AppID: "app", SessionID: "s1"}
	r.WakeTurn(in)
	<-first
	r.WakeTurn(in)
	r.WakeTurn(in)
	close(release)

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		done := dequeued == 2
		mu.Unlock()
		if done {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			t.Fatalf("dequeued=%d want 2 (depths=%v)", dequeued, depths)
			mu.Unlock()
		case <-time.After(5 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	// The turn that ran immediately is NOT queued: only the two behind it.
	if len(depths) != 2 || depths[0] != 1 || depths[1] != 2 {
		t.Errorf("queue depths = %v, want [1 2]", depths)
	}
}

// A session with a free lane must never touch the queue: that is the "direct
// path when nothing is running" contract.
func TestSessionRunner_DirectPathWhenIdle(t *testing.T) {
	done := make(chan struct{}, 1)
	r := newTestRunner(func(_ context.Context, _ runtime.TurnInput) error {
		done <- struct{}{}
		return nil
	})
	queued := 0
	r.queuedHook = func(_ runtime.TurnInput, _ int) { queued++ }

	r.WakeTurn(runtime.TurnInput{AppID: "app", SessionID: "s1"})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("turn never ran")
	}
	if queued != 0 {
		t.Errorf("idle session queued %d turns, want 0", queued)
	}
}

// Two sessions must not share a lane: a busy session A never delays session B.
func TestSessionRunner_SessionsAreIndependent(t *testing.T) {
	release := make(chan struct{})
	bRan := make(chan struct{}, 1)
	aStarted := make(chan struct{})
	var once sync.Once

	r := newTestRunner(func(_ context.Context, in runtime.TurnInput) error {
		if in.SessionID == "a" {
			once.Do(func() { close(aStarted) })
			<-release
			return nil
		}
		bRan <- struct{}{}
		return nil
	})

	r.WakeTurn(runtime.TurnInput{AppID: "app", SessionID: "a"})
	<-aStarted
	r.WakeTurn(runtime.TurnInput{AppID: "app", SessionID: "b"})

	select {
	case <-bRan:
	case <-time.After(5 * time.Second):
		t.Fatal("session b was blocked by session a")
	}
	close(release)
}
