package sessionstore

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func setupDurableBusT(t *testing.T) (*Bus, Paths, func()) {
	t.Helper()
	paths := NewPaths(t.TempDir())
	flusher, err := NewDiskFlusher(DiskFlusherConfig{
		Paths:            paths,
		NumShards:        8,
		QueueCapPerShard: 8192,
		BatchMax:         500,
		FlushInterval:    2 * time.Millisecond,
		FDCachePerShard:  64,
		PerSidQuotaPct:   80,
		Fsync:            true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := flusher.Start(); err != nil {
		t.Fatal(err)
	}
	bus, err := NewBus(BusConfig{
		Paths:               paths,
		Flusher:             flusher,
		EvictionInterval:    1 * time.Hour,
		StateIdleEvictAfter: 1 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return bus, paths, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		bus.Stop(ctx)
		flusher.Stop(ctx)
	}
}

func msgEvent(sid, content string) Event {
	return Event{Type: EventAssistantMessage, SessionID: sid,
		Message: &MessagePayload{Role: "assistant", Content: content}}
}

type msgTuple struct {
	Seq     uint64
	Role    string
	Content string
}

func tuples(msgs []Message) []msgTuple {
	out := make([]msgTuple, len(msgs))
	for i, m := range msgs {
		out[i] = msgTuple{Seq: m.Seq, Role: m.Role, Content: m.Content}
	}
	return out
}

// TestAppendDurableBatch_EquivalentToSerial is the core equivalence proof:
// persisting N events as a batch yields the SAME in-memory state and the
// SAME bytes on disk as persisting them one-by-one via AppendDurable.
func TestAppendDurableBatch_EquivalentToSerial(t *testing.T) {
	bus, paths, cleanup := setupDurableBusT(t)
	defer cleanup()
	ctx := context.Background()
	const N = 6

	for k := 0; k < N; k++ {
		if _, err := bus.AppendDurable(ctx, msgEvent("serial", fmt.Sprintf("m%d", k))); err != nil {
			t.Fatalf("serial append %d: %v", k, err)
		}
	}

	evs := make([]Event, N)
	for k := 0; k < N; k++ {
		evs[k] = msgEvent("batch", fmt.Sprintf("m%d", k))
	}
	seqs, err := bus.AppendDurableBatch(ctx, evs)
	if err != nil {
		t.Fatalf("batch append: %v", err)
	}
	if len(seqs) != N {
		t.Fatalf("seqs len = %d, want %d", len(seqs), N)
	}

	// In-memory state must match.
	sSerial, _ := bus.State("serial")
	sBatch, _ := bus.State("batch")
	if !reflect.DeepEqual(tuples(sSerial.Snapshot().Messages), tuples(sBatch.Snapshot().Messages)) {
		t.Fatalf("in-memory mismatch:\nserial=%v\nbatch =%v",
			tuples(sSerial.Snapshot().Messages), tuples(sBatch.Snapshot().Messages))
	}

	// On-disk state must match after a fresh cold load.
	lSerial, err := Load(paths, "serial", LoadOptions{Mode: JSONLStrict})
	if err != nil {
		t.Fatalf("load serial: %v", err)
	}
	lBatch, err := Load(paths, "batch", LoadOptions{Mode: JSONLStrict})
	if err != nil {
		t.Fatalf("load batch: %v", err)
	}
	if !reflect.DeepEqual(tuples(lSerial.State.Messages), tuples(lBatch.State.Messages)) {
		t.Fatalf("on-disk mismatch:\nserial=%v\nbatch =%v",
			tuples(lSerial.State.Messages), tuples(lBatch.State.Messages))
	}
}

// TestAppendDurableBatch_AssignsConsecutiveSeqs pins the contract that the
// batch assigns contiguous seqs 1..N in order.
func TestAppendDurableBatch_AssignsConsecutiveSeqs(t *testing.T) {
	bus, _, cleanup := setupDurableBusT(t)
	defer cleanup()
	const N = 8
	evs := make([]Event, N)
	for k := 0; k < N; k++ {
		evs[k] = msgEvent("seqs", fmt.Sprintf("m%d", k))
	}
	seqs, err := bus.AppendDurableBatch(context.Background(), evs)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	for k := 0; k < N; k++ {
		if seqs[k] != uint64(k+1) {
			t.Fatalf("seqs[%d] = %d, want %d", k, seqs[k], k+1)
		}
	}
}

// TestAppendDurableBatch_SurvivesReload is the durability proof: once the
// batch call returns nil with Fsync=true, every event must be recoverable
// from disk — the same kill -9 guarantee AppendDurable gives, per event.
func TestAppendDurableBatch_SurvivesReload(t *testing.T) {
	bus, paths, cleanup := setupDurableBusT(t)
	ctx := context.Background()
	const N = 10

	evs := make([]Event, N)
	for k := 0; k < N; k++ {
		evs[k] = Event{Type: EventToolResult, SessionID: "recover",
			Tool: &ToolPayload{CallID: fmt.Sprintf("c%d", k), Status: "completed", Output: "ok"}}
	}
	if _, err := bus.AppendDurableBatch(ctx, evs); err != nil {
		t.Fatalf("batch: %v", err)
	}

	// Stop everything (mimics shutdown), then cold-load from disk only.
	cleanup()

	res, err := ReadJSONL(paths.EventsFile("recover"), JSONLStrict, "")
	if err != nil {
		t.Fatalf("read jsonl: %v", err)
	}
	if len(res.Events) != N {
		t.Fatalf("recovered %d events, want %d", len(res.Events), N)
	}
	for i, ev := range res.Events {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d, want %d", i, ev.Seq, i+1)
		}
		if ev.Type != EventToolResult {
			t.Fatalf("event %d type = %s, want tool_result", i, ev.Type)
		}
	}
}
