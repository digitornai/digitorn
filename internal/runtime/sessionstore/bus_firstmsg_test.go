package sessionstore

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// newFirstMsgBus builds a real write-behind Bus on a temp dir. A SHORT
// flush interval is used deliberately : it maximises the chance the
// flusher races a freshly-enqueued event onto disk before AppendDurable
// cold-loads the session — the exact condition that used to duplicate
// the first message.
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

// TestBus_NoDuplicateProjection appends N messages and asserts the
// projected state has EXACTLY N messages with strictly-increasing,
// unique seqs. The old write-behind/cold-load race duplicated the first
// message (seq 1 projected twice).
func TestBus_NoDuplicateProjection(t *testing.T) {
	bus := newFirstMsgBus(t)
	const sid = "sess-firstmsg"
	const n = 8
	ctx := context.Background()
	for i := 1; i <= n; i++ {
		// A tiny pause lets the flusher commit the prior event to disk,
		// recreating the race window for the FIRST event reliably.
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

// TestBus_FirstMessageOnceUnderTightLoop hammers the first-event window :
// a brand-new session, single append, immediate read. The very first
// message must appear exactly once.
func TestBus_FirstMessageOnceUnderTightLoop(t *testing.T) {
	for trial := 0; trial < 20; trial++ {
		bus := newFirstMsgBus(t)
		sid := fmt.Sprintf("sess-tight-%d", trial)
		// Let the flusher idle so the first enqueue is likely to flush
		// during the cold-load window.
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
