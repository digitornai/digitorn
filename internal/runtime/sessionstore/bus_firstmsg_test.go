package sessionstore

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func newFirstMsgBus(t *testing.T) *Bus {
	t.Helper()
	paths := NewPaths(t.TempDir())
	flusher, err := NewDiskFlusher(DiskFlusherConfig{
		Paths: paths, NumShards: 2, QueueCapPerShard: 4096,
		BatchMax: 100, FlushInterval: time.Millisecond, Fsync: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := flusher.Start(); err != nil {
		t.Fatal(err)
	}
	bus, err := NewBus(BusConfig{
		Paths: paths, Flusher: flusher,
		EvictionInterval: time.Hour, StateIdleEvictAfter: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		bus.Stop(ctx)
		flusher.Stop(ctx)
	})
	return bus
}

func TestBus_NoDuplicateProjection(t *testing.T) {
	bus := newFirstMsgBus(t)
	const sid = "sess-firstmsg"
	const n = 8
	ctx := context.Background()
	for i := 1; i <= n; i++ {
		time.Sleep(3 * time.Millisecond)
		if _, err := bus.AppendDurable(ctx, Event{
			Type:      EventUserMessage,
			SessionID: sid,
			Message:   &MessagePayload{Role: "user", Content: fmt.Sprintf("msg %d", i)},
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	st, err := bus.State(sid)
	if err != nil {
		t.Fatal(err)
	}
	snap := st.Snapshot()

	if len(snap.Messages) != n {
		t.Errorf("got %d messages, want %d (duplication?)", len(snap.Messages), n)
	}
	seen := map[uint64]bool{}
	var prev uint64
	for i, m := range snap.Messages {
		if seen[m.Seq] {
			t.Errorf("duplicate seq %d at index %d (content=%q)", m.Seq, i, m.Content)
		}
		seen[m.Seq] = true
		if m.Seq <= prev {
			t.Errorf("non-increasing seq at index %d: %d after %d", i, m.Seq, prev)
		}
		prev = m.Seq
	}
	if snap.EventCount != n {
		t.Errorf("EventCount = %d, want %d (double-count?)", snap.EventCount, n)
	}
}

func TestBus_FirstMessageOnceUnderTightLoop(t *testing.T) {
	for trial := 0; trial < 20; trial++ {
		bus := newFirstMsgBus(t)
		sid := fmt.Sprintf("sess-tight-%d", trial)
		if _, err := bus.AppendDurable(context.Background(), Event{
			Type: EventUserMessage, SessionID: sid,
			Message: &MessagePayload{Role: "user", Content: "first"},
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
		st, err := bus.State(sid)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(st.Snapshot().Messages); got != 1 {
			t.Fatalf("trial %d: first message projected %d times, want 1", trial, got)
		}
	}
}
