package preview

import (
	"context"
	"testing"
	"time"
)

// Speed and reliability are features here: the agent runs these inside its own
// turn, so a second of avoidable latency per action is a second the user waits,
// and a slow failure is worse than a fast one.

func TestWaitReleasesTheInstantACommandArrives(t *testing.T) {
	s := NewStore()
	s.Report("app", "A", Snapshot{URL: "http://a/"})

	released := make(chan []Command, 1)
	go func() { released <- s.Wait(context.Background(), "app", "A", 5*time.Second) }()

	// Let the waiter park, then queue work as the agent would.
	time.Sleep(30 * time.Millisecond)
	go func() { _, _ = s.Submit(context.Background(), "app", "A", Command{ID: "c1", Do: "click"}) }()

	start := time.Now()
	select {
	case cmds := <-released:
		took := time.Since(start)
		if len(cmds) != 1 || cmds[0].ID != "c1" {
			t.Fatalf("got %+v", cmds)
		}
		if took > 250*time.Millisecond {
			t.Errorf("took %v to hand over a queued command; the point of holding the request is that this is immediate", took)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a queued command never woke the parked page")
	}
}

func TestWaitReturnsEmptyAfterItsBudget(t *testing.T) {
	s := NewStore()
	s.Report("app", "A", Snapshot{})

	start := time.Now()
	cmds := s.Wait(context.Background(), "app", "A", 120*time.Millisecond)
	took := time.Since(start)

	if len(cmds) != 0 {
		t.Fatalf("got %+v, want nothing", cmds)
	}
	if took < 100*time.Millisecond {
		t.Errorf("returned after %v — it must actually hold, or we are back to busy polling", took)
	}
	if took > time.Second {
		t.Errorf("held for %v, well past its budget", took)
	}
}

func TestWaitTakesWorkAlreadyQueuedWithoutParking(t *testing.T) {
	s := NewStore()
	go func() { _, _ = s.Submit(context.Background(), "app", "A", Command{ID: "c1", Do: "observe"}) }()
	waitFor(t, func() bool { return len(s.peekQueue("app", "A")) == 1 })

	start := time.Now()
	cmds := s.Wait(context.Background(), "app", "A", 5*time.Second)
	if len(cmds) != 1 {
		t.Fatalf("got %+v", cmds)
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Errorf("waited %v on work that was already there", took)
	}
}

func TestWaitEndsWithItsRequest(t *testing.T) {
	// The page navigated away or the tab closed: the held request is cancelled
	// and the goroutine behind it must not linger.
	s := NewStore()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Wait(ctx, "app", "A", 10*time.Second); close(done) }()

	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelling the request left the wait hanging")
	}
}

func TestSubmitFailsFastWhenThePreviewIsGone(t *testing.T) {
	s := NewStore()
	// A page that checked in long ago and never came back.
	s.Report("app", "A", Snapshot{})
	s.mu.Lock()
	s.sessions[key{"app", "A"}].lastSeen = time.Now().Add(-2 * staleAfter)
	s.mu.Unlock()

	start := time.Now()
	_, err := s.Submit(context.Background(), "app", "A", Command{ID: "c1", Do: "observe"})
	took := time.Since(start)

	if err != ErrNoPreview {
		t.Fatalf("err = %v, want ErrNoPreview", err)
	}
	if took > 200*time.Millisecond {
		t.Errorf("took %v to report a preview we already knew was gone; the agent should not lose its turn to a timeout", took)
	}
}

func TestWaitersOfDifferentSessionsDoNotWakeEachOther(t *testing.T) {
	s := NewStore()
	s.Report("app", "A", Snapshot{})
	s.Report("app", "B", Snapshot{})

	woke := make(chan []Command, 1)
	go func() { woke <- s.Wait(context.Background(), "app", "B", 400*time.Millisecond) }()
	time.Sleep(30 * time.Millisecond)

	go func() { _, _ = s.Submit(context.Background(), "app", "A", Command{ID: "c1", Do: "observe"}) }()

	select {
	case cmds := <-woke:
		if len(cmds) != 0 {
			t.Fatalf("session B was handed session A's work: %+v", cmds)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session B never returned")
	}
}

// The bug this pins cost the agent fifteen seconds per call in production.
//
// Draining the queue and reading the wake channel used to be two separate lock
// acquisitions. A command queued in between closed the channel the waiter had
// not read yet; the waiter then parked on its freshly-created replacement and
// slept out the entire budget while the work sat in the queue. The caller hit
// its own timeout and reported no preview — the tool simply hung.
//
// Racing the two on every iteration makes the window hit reliably.
func TestWaitNeverMissesAWakeItRacedWith(t *testing.T) {
	const rounds = 400
	for i := 0; i < rounds; i++ {
		s := NewStore()
		s.Report("app", "A", Snapshot{})

		got := make(chan []Command, 1)
		start := make(chan struct{})
		go func() {
			<-start
			got <- s.Wait(context.Background(), "app", "A", 150*time.Millisecond)
		}()
		go func() {
			<-start
			s.mu.Lock()
			st := s.at(key{"app", "A"})
			st.queue = append(st.queue, Command{ID: "c1", Do: "observe"})
			st.ring()
			s.mu.Unlock()
		}()
		close(start)

		select {
		case cmds := <-got:
			if len(cmds) != 1 {
				t.Fatalf("round %d: the waiter came back empty while a command sat in the queue — "+
					"the wake signal was lost, which is a 15s hang for the agent", i)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("round %d: the waiter never returned", i)
		}
	}
}
