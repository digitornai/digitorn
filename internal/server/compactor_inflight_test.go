package server

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// newCompactorTestBusWithPaths is newCompactorTestBus but returns the on-disk
// Paths too, so a test can read the raw durable JSONL back and inspect the
// exact events + seqs the compactor wrote.
func newCompactorTestBusWithPaths(t *testing.T) (*sessionstore.Bus, sessionstore.Paths) {
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
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		bus.Stop(ctx)
		flusher.Stop(ctx)
	})
	return bus, paths
}

// TestCompactor_EmitsStartThenEndMarkerInSeqOrder is the proof Paul asked for :
// a compaction emits a START (context_compacting) marker BEFORE the work and an
// END (context_compacted) marker after — both durable, and the START's seq is
// STRICTLY below the END's, after the last real message. A client can show a
// "compacting…" indicator on START and clear it on END, and the ordering holds
// on replay because both went through the monotone AppendDurable path.
func TestCompactor_EmitsStartThenEndMarkerInSeqOrder(t *testing.T) {
	bus, paths := newCompactorTestBusWithPaths(t)
	sid := "sess-compact-pair"
	seedMessages(t, bus, sid, 8) // seqs 1..8

	c := newContextCompactor(bus, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.CompactSession(context.Background(), sid, "truncate", 2); err != nil {
		t.Fatalf("CompactSession: %v", err)
	}

	// Flush the write-behind queue and read the durable log back.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := bus.FlushPending(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	res, err := sessionstore.ReadJSONL(paths.EventsFile(sid), sessionstore.JSONLStrict, "")
	if err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}

	var start, end *sessionstore.Event
	for i := range res.Events {
		switch res.Events[i].Type {
		case sessionstore.EventContextCompacting:
			if start != nil {
				t.Fatal("more than one START marker — must be exactly one per pass")
			}
			start = &res.Events[i]
		case sessionstore.EventContextCompacted:
			if end != nil {
				t.Fatal("more than one END marker — must be exactly one per pass")
			}
			end = &res.Events[i]
		}
	}
	if start == nil {
		t.Fatal("no context_compacting (START) marker was emitted")
	}
	if end == nil {
		t.Fatal("no context_compacted (END) marker was emitted")
	}

	// THE invariant : start strictly before end, both after the last message.
	if !(start.Seq < end.Seq) {
		t.Fatalf("seq order violated: start.Seq=%d must be < end.Seq=%d", start.Seq, end.Seq)
	}
	if start.Seq <= 8 {
		t.Fatalf("START seq %d should come after the 8 seeded messages", start.Seq)
	}

	// START carries the up-front info a client needs to render the indicator.
	if start.CtxCompact == nil {
		t.Fatal("START marker has no payload — client cannot show what's compacting")
	}
	if start.CtxCompact.MessagesDropped != 6 || start.CtxCompact.CutoffSeq != 6 {
		t.Errorf("START payload wrong: dropped=%d cutoff=%d, want 6/6",
			start.CtxCompact.MessagesDropped, start.CtxCompact.CutoffSeq)
	}
	if start.CtxCompact.Strategy != "truncate" {
		t.Errorf("START strategy = %q, want truncate", start.CtxCompact.Strategy)
	}
	// END carries the outcome.
	if end.CtxCompact == nil || end.CtxCompact.CutoffSeq != 6 {
		t.Fatalf("END payload wrong: %+v", end.CtxCompact)
	}

	// Final projected state : compaction is no longer inflight, view recorded.
	snap := mustSnap(t, bus, sid)
	if snap.CompactionInflight {
		t.Error("CompactionInflight must be false after the END marker")
	}
	if snap.ContextCompaction == nil || snap.ContextCompaction.CutoffSeq != 6 {
		t.Fatalf("no view cutoff recorded after compaction: %+v", snap.ContextCompaction)
	}
	t.Logf("PROVEN: START seq=%d < END seq=%d, paired & durable; inflight cleared on END", start.Seq, end.Seq)
}

// TestCompactor_NoStartMarkerOnNoOp : a pass that drops nothing must emit
// NEITHER marker — so a client never sees a dangling "compacting…" with no end.
func TestCompactor_NoStartMarkerOnNoOp(t *testing.T) {
	bus, paths := newCompactorTestBusWithPaths(t)
	sid := "sess-compact-noop"
	seedMessages(t, bus, sid, 2) // fewer than keep_last → nothing to drop

	c := newContextCompactor(bus, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.CompactSession(context.Background(), sid, "truncate", 10); err != nil {
		t.Fatalf("CompactSession: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := bus.FlushPending(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	res, err := sessionstore.ReadJSONL(paths.EventsFile(sid), sessionstore.JSONLStrict, "")
	if err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}
	for i := range res.Events {
		if res.Events[i].Type == sessionstore.EventContextCompacting {
			t.Fatal("no-op pass must NOT emit a START marker (would dangle the spinner)")
		}
		if res.Events[i].Type == sessionstore.EventContextCompacted {
			t.Fatal("no-op pass must NOT emit an END marker")
		}
	}
	if mustSnap(t, bus, sid).CompactionInflight {
		t.Fatal("no-op pass must leave CompactionInflight false")
	}
}
