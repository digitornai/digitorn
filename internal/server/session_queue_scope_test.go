package server

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// The queue hooks append durable events; the bridge fans each to its session's
// room by SessionID, so isolation is structural. These drive the hooks through
// a real bus and assert each session's projection independently — one session's
// queue can never carry another's row.

func newQueueTestDaemon(t *testing.T) (*Daemon, func()) {
	t.Helper()
	paths := sessionstore.NewPaths(t.TempDir())
	flusher, err := sessionstore.NewDiskFlusher(sessionstore.DiskFlusherConfig{
		Paths: paths, NumShards: 2, QueueCapPerShard: 4096,
		BatchMax: 100, FlushInterval: 2 * time.Millisecond, Fsync: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := flusher.Start(); err != nil {
		t.Fatal(err)
	}
	bus, err := sessionstore.NewBus(sessionstore.BusConfig{
		Paths: paths, Flusher: flusher,
		EvictionInterval: time.Hour, StateIdleEvictAfter: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{
		sessionStore: bus,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		bus.Stop(ctx)
		flusher.Stop(ctx)
	}
	return d, cleanup
}

func queueOf(t *testing.T, d *Daemon, sid string) []sessionstore.QueueEntry {
	t.Helper()
	st, err := d.sessionStore.State(sid)
	if err != nil {
		t.Fatalf("state %s: %v", sid, err)
	}
	st.RLock()
	defer st.RUnlock()
	return append([]sessionstore.QueueEntry(nil), st.Queue...)
}

func TestQueueHooks_RowLandsInItsOwnSessionOnly(t *testing.T) {
	d, done := newQueueTestDaemon(t)
	defer done()

	d.onTurnQueued(runtime.TurnInput{
		AppID: "app", SessionID: "session-a", UserID: "u",
		ClientMessageID: "cid-a", Message: "bonjour",
	}, 1)
	d.onTurnQueued(runtime.TurnInput{
		AppID: "app", SessionID: "session-b", UserID: "u",
		ClientMessageID: "cid-b", Message: "salut",
	}, 1)

	a := queueOf(t, d, "session-a")
	b := queueOf(t, d, "session-b")
	if len(a) != 1 || a[0].CorrelationID != "cid-a" || a[0].Message != "bonjour" {
		t.Fatalf("session-a queue = %+v", a)
	}
	if len(b) != 1 || b[0].CorrelationID != "cid-b" {
		t.Fatalf("session-b queue = %+v", b)
	}
	if a[0].Status != "queued" || a[0].Position != 1 {
		t.Errorf("row a = %+v, want queued/pos1", a[0])
	}
}

func TestQueueHooks_DequeueRemovesTheRow(t *testing.T) {
	d, done := newQueueTestDaemon(t)
	defer done()

	in := runtime.TurnInput{
		AppID: "app", SessionID: "s1", UserID: "u",
		ClientMessageID: "cid-1", Message: "un",
	}
	d.onTurnQueued(in, 1)
	if q := queueOf(t, d, "s1"); len(q) != 1 {
		t.Fatalf("after queued: %+v", q)
	}

	// The message's turn starts: the row must leave the queue.
	d.onTurnDequeued(in)
	if q := queueOf(t, d, "s1"); len(q) != 0 {
		t.Fatalf("after dequeue: %+v, want empty", q)
	}
}

// A dequeue with no client message id (proactive wake path) writes nothing.
func TestQueueHooks_NoOpWithoutCorrelation(t *testing.T) {
	d, done := newQueueTestDaemon(t)
	defer done()

	d.onTurnDequeued(runtime.TurnInput{AppID: "app", SessionID: "s1"})
	if q := queueOf(t, d, "s1"); len(q) != 0 {
		t.Errorf("a wake with no correlation created a row: %+v", q)
	}
}
