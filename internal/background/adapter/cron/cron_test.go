package cron

import (
	"context"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/background/adapter"
)

func mustParse(t *testing.T, e string) *Schedule {
	t.Helper()
	s, err := Parse(e)
	if err != nil {
		t.Fatalf("Parse(%q): %v", e, err)
	}
	return s
}

func TestParse_Rejects(t *testing.T) {
	for _, bad := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "*/0 * * * *", "5-2 * * * *", "a * * * *"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

func TestNext_EveryMinute(t *testing.T) {
	s := mustParse(t, "* * * * *")
	at := time.Date(2026, 1, 2, 3, 4, 30, 0, time.UTC)
	if got := s.Next(at); !got.Equal(time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC)) {
		t.Fatalf("next = %v", got)
	}
}

func TestNext_SpecificTime(t *testing.T) {
	// 09:00 on weekdays (Mon-Fri).
	s := mustParse(t, "0 9 * * 1-5")
	// Saturday 2026-01-03 10:00 → next is Monday 2026-01-05 09:00.
	at := time.Date(2026, 1, 3, 10, 0, 0, 0, time.UTC)
	got := s.Next(at)
	want := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next = %v, want %v", got, want)
	}
}

func TestNext_StepAndList(t *testing.T) {
	s := mustParse(t, "*/15 * * * *") // 0,15,30,45
	at := time.Date(2026, 1, 1, 0, 7, 0, 0, time.UTC)
	if got := s.Next(at); got.Minute() != 15 {
		t.Fatalf("next minute = %d, want 15", got.Minute())
	}
	s2 := mustParse(t, "5,35 * * * *")
	at2 := time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC)
	if got := s2.Next(at2); got.Minute() != 35 {
		t.Fatalf("next minute = %d, want 35", got.Minute())
	}
}

func TestNext_VixieDayOr(t *testing.T) {
	// Both dom (15) and dow (Mon=1) restricted → fires when EITHER matches.
	s := mustParse(t, "0 0 15 * 1")
	// From Jan 1 2026 (Thu): the 15th OR a Monday, whichever first. Mondays in
	// Jan 2026: 5,12,19,26. The 15th is Thu. So next is Mon Jan 5.
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got := s.Next(at)
	if got.Day() != 5 {
		t.Fatalf("vixie-or next day = %d, want 5 (first Monday)", got.Day())
	}
}

func TestAdapter_Fires(t *testing.T) {
	s := mustParse(t, "* * * * *")
	a := New([]Provider{{Name: "tick", Schedule: s}})
	// Fixed past clock → the computed next-minute is in the past → timer fires
	// immediately, exercising the run loop without waiting a real minute.
	a.now = func() time.Time { return time.Date(2020, 1, 1, 0, 0, 30, 0, time.UTC) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan adapter.Event, 1)
	sink := func(c context.Context, ev adapter.Event) error {
		select {
		case ch <- ev:
		case <-c.Done():
		}
		return nil
	}
	go func() { _ = a.Start(ctx, sink) }()

	select {
	case ev := <-ch:
		if ev.Provider != "tick" || ev.Adapter != "cron" || ev.DedupKey == "" {
			t.Fatalf("bad event: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cron did not fire")
	}
	cancel()
}
