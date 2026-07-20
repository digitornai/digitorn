package preview

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Isolation is the requirement this feature lives or dies on: a session must
// never see another session's app. These tests attack that from every angle the
// API exposes — reading, writing, command delivery and command completion.

func TestSessionsAreIsolated(t *testing.T) {
	s := NewStore()
	s.Report("app", "A", Snapshot{URL: "http://a/", Text: "secret A"})
	s.Report("app", "B", Snapshot{URL: "http://b/", Text: "secret B"})

	a, ok := s.Observe("app", "A")
	if !ok || a.Text != "secret A" {
		t.Fatalf("session A got %+v", a)
	}
	b, ok := s.Observe("app", "B")
	if !ok || b.Text != "secret B" {
		t.Fatalf("session B got %+v", b)
	}
	if a.URL == b.URL {
		t.Fatal("two sessions collapsed onto the same state")
	}
}

func TestSameSessionIDUnderAnotherAppIsADifferentPreview(t *testing.T) {
	// Session ids are not globally unique across apps; the key must carry both
	// or one app could read another's preview by reusing an id.
	s := NewStore()
	s.Report("app-one", "S", Snapshot{Text: "one"})
	s.Report("app-two", "S", Snapshot{Text: "two"})

	if got, _ := s.Observe("app-one", "S"); got.Text != "one" {
		t.Errorf("app-one sees %q", got.Text)
	}
	if got, _ := s.Observe("app-two", "S"); got.Text != "two" {
		t.Errorf("app-two sees %q", got.Text)
	}
}

func TestUnknownSessionReportsNothingRatherThanSomeoneElsesState(t *testing.T) {
	s := NewStore()
	s.Report("app", "A", Snapshot{Text: "secret A"})

	if snap, seen := s.Observe("app", "ghost"); seen || snap.Text != "" {
		t.Fatalf("an unknown session must observe nothing, got %+v (seen=%v)", snap, seen)
	}
	if s.Live("app", "ghost") {
		t.Error("an unknown session must not look live")
	}
}

func TestCommandsGoOnlyToTheirOwnSession(t *testing.T) {
	s := NewStore()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.Submit(context.Background(), "app", "A", Command{ID: "c1", Do: "observe"})
	}()

	waitFor(t, func() bool { return len(s.peekQueue("app", "A")) == 1 })

	if got := s.Take("app", "B"); len(got) != 0 {
		t.Fatalf("session B collected session A's commands: %+v", got)
	}
	got := s.Take("app", "A")
	if len(got) != 1 || got[0].ID != "c1" {
		t.Fatalf("session A did not get its own command: %+v", got)
	}

	// B answering with A's command id must not release A's caller either.
	s.Complete("app", "B", "c1", Snapshot{Text: "from B"})
	select {
	case <-done:
		t.Fatal("session B completed a command belonging to session A")
	case <-time.After(150 * time.Millisecond):
	}

	s.Complete("app", "A", "c1", Snapshot{Text: "from A"})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the rightful session never released the caller")
	}
}

func TestSubmitReturnsThePostActionState(t *testing.T) {
	s := NewStore()
	type res struct {
		snap Snapshot
		err  error
	}
	out := make(chan res, 1)
	go func() {
		snap, err := s.Submit(context.Background(), "app", "A", Command{ID: "c1", Do: "click", Ref: "e1"})
		out <- res{snap, err}
	}()
	waitFor(t, func() bool { return len(s.peekQueue("app", "A")) == 1 })
	s.Take("app", "A")
	s.Complete("app", "A", "c1", Snapshot{URL: "http://a/#/done", Text: "Merci !"})

	r := <-out
	if r.err != nil {
		t.Fatalf("err = %v", r.err)
	}
	if r.snap.Text != "Merci !" || r.snap.URL != "http://a/#/done" {
		t.Fatalf("caller got the wrong state: %+v", r.snap)
	}
}

func TestSubmitTimesOutWhenNoPageAnswers(t *testing.T) {
	s := NewStore()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := s.Submit(ctx, "app", "A", Command{ID: "c1", Do: "observe"})
	if err == nil {
		t.Fatal("a command nobody picks up must not hang forever")
	}
	if q := s.peekQueue("app", "A"); len(q) != 0 {
		t.Errorf("an abandoned command must leave the queue, still holds %+v", q)
	}
}

func TestErrorsAccumulateDeduplicatedAndBounded(t *testing.T) {
	s := NewStore()
	boom := RuntimeError{Kind: "error", Message: "x is not a function", Line: 12}
	for i := 0; i < 5; i++ {
		s.Report("app", "A", Snapshot{Errors: []RuntimeError{boom}})
	}
	snap, _ := s.Observe("app", "A")
	if len(snap.Errors) != 1 {
		t.Fatalf("the same failure must collapse into one entry, got %d", len(snap.Errors))
	}
	if snap.Errors[0].Count != 5 {
		t.Errorf("count = %d, want 5", snap.Errors[0].Count)
	}

	for i := 0; i < maxErrors*2; i++ {
		s.Report("app", "A", Snapshot{Errors: []RuntimeError{{Kind: "error", Message: string(rune('a' + i%26)), Line: i}}})
	}
	snap, _ = s.Observe("app", "A")
	if len(snap.Errors) > maxErrors {
		t.Fatalf("errors grew unbounded: %d", len(snap.Errors))
	}
}

func TestReportReplacesStateButKeepsErrors(t *testing.T) {
	s := NewStore()
	s.Report("app", "A", Snapshot{URL: "http://a/#/one", Errors: []RuntimeError{{Kind: "error", Message: "boom"}}})
	s.Report("app", "A", Snapshot{URL: "http://a/#/two"})

	snap, _ := s.Observe("app", "A")
	if snap.URL != "http://a/#/two" {
		t.Errorf("URL = %q, the latest report should win", snap.URL)
	}
	if len(snap.Errors) != 1 {
		t.Errorf("a failure must survive the next report, or the agent never sees it")
	}

	s.ClearErrors("app", "A")
	if snap, _ := s.Observe("app", "A"); len(snap.Errors) != 0 {
		t.Error("ClearErrors left failures behind")
	}
}

func TestQueueIsBounded(t *testing.T) {
	s := NewStore()
	for i := 0; i < maxQueued; i++ {
		go func(i int) {
			_, _ = s.Submit(context.Background(), "app", "A", Command{ID: string(rune('a' + i)), Do: "observe"})
		}(i)
	}
	waitFor(t, func() bool { return len(s.peekQueue("app", "A")) == maxQueued })

	_, err := s.Submit(context.Background(), "app", "A", Command{ID: "overflow", Do: "observe"})
	if err != ErrBusy {
		t.Fatalf("err = %v, want ErrBusy — an unbounded queue lets a stuck page pile up work", err)
	}
}

func TestConcurrentSessionsDoNotRace(t *testing.T) {
	s := NewStore()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sess := string(rune('A' + i%5))
			s.Report("app", sess, Snapshot{Text: sess})
			s.Take("app", sess)
			s.Observe("app", sess)
			s.Live("app", sess)
			s.ClearErrors("app", sess)
		}(i)
	}
	wg.Wait()
}

// peekQueue reads the pending commands without consuming them; tests only.
func (s *Store) peekQueue(app, session string) []Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.sessions[key{app, session}]
	if !ok {
		return nil
	}
	return append([]Command(nil), st.queue...)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}
