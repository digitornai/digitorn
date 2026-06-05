package sessionstore

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func benchSetupSession(b *testing.B, paths Paths, sid string, n int) {
	b.Helper()
	if err := os.MkdirAll(paths.SessionDir(sid), 0o755); err != nil {
		b.Fatal(err)
	}
	now := time.Now().UnixNano()
	w, err := OpenJSONLAppend(paths.EventsFile(sid), false)
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()
	batch := make([]Event, 0, 500)
	for i := 1; i <= n; i++ {
		var typ EventType
		switch i % 4 {
		case 0:
			typ = EventToolResult
		case 1:
			typ = EventUserMessage
		case 2:
			typ = EventToolCall
		default:
			typ = EventAssistantMessage
		}
		ev := Event{
			Seq: uint64(i), Type: typ, TsUnixNano: now + int64(i), SessionID: sid,
		}
		switch typ {
		case EventUserMessage, EventAssistantMessage:
			ev.Message = &MessagePayload{Role: "user", Content: fmt.Sprintf("event %d with some realistic content length here", i)}
		case EventToolCall:
			ev.Tool = &ToolPayload{CallID: fmt.Sprintf("c%d", i), Name: "read", Status: "pending"}
		case EventToolResult:
			ev.Tool = &ToolPayload{CallID: fmt.Sprintf("c%d", i-1), Status: "completed", Output: "ok"}
		}
		batch = append(batch, ev)
		if len(batch) >= 500 {
			w.Write(batch)
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		w.Write(batch)
	}
	w.Flush()
}

func BenchmarkColdLoad_100Events_NoSnapshot(b *testing.B) {
	paths := NewPaths(b.TempDir())
	sid := "bench-100"
	benchSetupSession(b, paths, sid, 100)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkColdLoad_1000Events_NoSnapshot(b *testing.B) {
	paths := NewPaths(b.TempDir())
	sid := "bench-1000"
	benchSetupSession(b, paths, sid, 1000)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkColdLoad_10000Events_NoSnapshot(b *testing.B) {
	paths := NewPaths(b.TempDir())
	sid := "bench-10000"
	benchSetupSession(b, paths, sid, 10000)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkColdLoad_WithSnapshotPlus50Tail(b *testing.B) {
	paths := NewPaths(b.TempDir())
	sid := "bench-snap"
	benchSetupSession(b, paths, sid, 1000)

	// Compact first.
	st, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
	if err != nil {
		b.Fatal(err)
	}
	c := NewCompactor(CompactorConfig{Paths: paths})
	if _, err := c.Compact(context.Background(), st.State, CompactOptions{TruncateMode: TruncateSync}); err != nil {
		b.Fatal(err)
	}
	// Add 50 events past the snapshot.
	w, err := OpenJSONLAppend(paths.EventsFile(sid), false)
	if err != nil {
		b.Fatal(err)
	}
	alloc, _ := c.Seqs().For(sid)
	now := time.Now().UnixNano()
	batch := make([]Event, 0, 50)
	for i := 0; i < 50; i++ {
		batch = append(batch, Event{
			Seq: alloc.Next(), Type: EventUserMessage, TsUnixNano: now + int64(i), SessionID: sid,
			Message: &MessagePayload{Role: "user", Content: "tail event"},
		})
	}
	w.Write(batch)
	w.Close()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict}); err != nil {
			b.Fatal(err)
		}
	}
}

func setupBenchBus(b *testing.B) (*Bus, *DiskFlusher, func()) {
	b.Helper()
	paths := NewPaths(b.TempDir())
	flusher, err := NewDiskFlusher(DiskFlusherConfig{
		Paths:            paths,
		NumShards:        8,
		QueueCapPerShard: 8192,
		BatchMax:         500,
		FlushInterval:    2 * time.Millisecond,
		FDCachePerShard:  64,
		PerSidQuotaPct:   80,
	})
	if err != nil {
		b.Fatal(err)
	}
	if err := flusher.Start(); err != nil {
		b.Fatal(err)
	}
	bus, err := NewBus(BusConfig{
		Paths:               paths,
		Flusher:             flusher,
		EvictionInterval:    1 * time.Hour,
		StateIdleEvictAfter: 1 * time.Hour,
	})
	if err != nil {
		b.Fatal(err)
	}
	if err := bus.Start(context.Background()); err != nil {
		b.Fatal(err)
	}
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		bus.Stop(ctx)
		flusher.Stop(ctx)
	}
	return bus, flusher, cleanup
}

func BenchmarkBusAppend_HotPath(b *testing.B) {
	bus, flusher, cleanup := setupBenchBus(b)
	defer cleanup()
	sid := "bench-hot"
	ev := Event{Type: EventUserMessage, SessionID: sid, Message: &MessagePayload{Role: "user", Content: "hello world"}}

	flushEvery := 4096
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := bus.Append(context.Background(), ev); err != nil {
			b.Fatal(err)
		}
		if (i+1)%flushEvery == 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := flusher.Flush(ctx); err != nil {
				cancel()
				b.Fatal(err)
			}
			cancel()
		}
	}
}

func BenchmarkBusAppend_Parallel_DifferentSessions(b *testing.B) {
	bus, flusher, cleanup := setupBenchBus(b)
	defer cleanup()

	flushEvery := 4096
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		sid := fmt.Sprintf("bench-par-%p", pb)
		ev := Event{Type: EventUserMessage, SessionID: sid, Message: &MessagePayload{Role: "user", Content: "x"}}
		for pb.Next() {
			if _, err := bus.Append(context.Background(), ev); err != nil {
				b.Fatal(err)
			}
			i++
			if i%flushEvery == 0 {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = flusher.Flush(ctx)
				cancel()
			}
		}
	})
}

func BenchmarkSnapshotWrite_JSON_1000Events(b *testing.B) {
	paths := NewPaths(b.TempDir())
	sid := "bench-snap-json"
	benchSetupSession(b, paths, sid, 1000)
	st, _ := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
	snap := st.State.Snapshot()
	snap.CutoffSeq = snap.LastSeq

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dir := paths.SessionDir(sid)
		if _, err := WriteSnapshotAtomic(dir, snap, SnapshotJSON, false); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSnapshotWrite_Binary_1000Events(b *testing.B) {
	paths := NewPaths(b.TempDir())
	sid := "bench-snap-bin"
	benchSetupSession(b, paths, sid, 1000)
	st, _ := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
	snap := st.State.Snapshot()
	snap.CutoffSeq = snap.LastSeq

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dir := paths.SessionDir(sid)
		if _, err := WriteSnapshotAtomic(dir, snap, SnapshotBinary, false); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompaction_1000Events(b *testing.B) {
	paths := NewPaths(b.TempDir())
	c := NewCompactor(CompactorConfig{Paths: paths})

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		sid := fmt.Sprintf("bench-comp-%d", i)
		benchSetupSession(b, paths, sid, 1000)
		st, _ := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
		b.StartTimer()
		if _, err := c.Compact(context.Background(), st.State, CompactOptions{TruncateMode: TruncateSync}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSeqAllocator_Next(b *testing.B) {
	a := NewSeqAllocator(0)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		a.Next()
	}
}

func BenchmarkSeqAllocator_NextParallel(b *testing.B) {
	a := NewSeqAllocator(0)
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			a.Next()
		}
	})
}

func BenchmarkBuildHistory_500Events(b *testing.B) {
	paths := NewPaths(b.TempDir())
	sid := "bench-view"
	benchSetupSession(b, paths, sid, 500)
	st, _ := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
	jres, _ := ReadJSONL(paths.EventsFile(sid), JSONLStrict, "")

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = BuildHistory(st.State, st.State.Messages, jres.Events, ViewOptions{IncludePayload: true, MaxEvents: 500})
	}
}
