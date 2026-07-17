package sessionstore

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSeq_AllocatorMonotonicSingleThread(t *testing.T) {
	a := NewSeqAllocator(0)
	prev := uint64(0)
	for i := 0; i < 100_000; i++ {
		got := a.Next()
		if got != prev+1 {
			t.Fatalf("non-monotonic at i=%d: prev=%d got=%d", i, prev, got)
		}
		prev = got
	}
	if a.Current() != 100_000 {
		t.Fatalf("current after 100k Next: %d", a.Current())
	}
}

func TestSeq_AllocatorConcurrentUnique(t *testing.T) {
	a := NewSeqAllocator(0)
	const goroutines = 64
	const perGoroutine = 10_000
	seen := sync.Map{}
	var dupes atomic.Int64
	var maxSeq atomic.Uint64

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				s := a.Next()
				if _, loaded := seen.LoadOrStore(s, struct{}{}); loaded {
					dupes.Add(1)
				}
				for {
					cur := maxSeq.Load()
					if s <= cur || maxSeq.CompareAndSwap(cur, s) {
						break
					}
				}
			}
		}()
	}
	wg.Wait()

	if dupes.Load() != 0 {
		t.Fatalf("seq duplicates: %d", dupes.Load())
	}
	if maxSeq.Load() != goroutines*perGoroutine {
		t.Fatalf("max seq mismatch: %d vs %d", maxSeq.Load(), goroutines*perGoroutine)
	}
	if a.Current() != uint64(goroutines*perGoroutine) {
		t.Fatalf("current: %d", a.Current())
	}
}

func TestSeq_BumpNeverGoesBackwards(t *testing.T) {
	a := NewSeqAllocator(100)
	a.Bump(50)
	if a.Current() != 100 {
		t.Fatalf("bump backwards: %d", a.Current())
	}
	a.Bump(200)
	if a.Current() != 200 {
		t.Fatalf("bump forward: %d", a.Current())
	}
	if got := a.Next(); got != 201 {
		t.Fatalf("next after bump: %d", got)
	}
}

func TestSeq_RecoverFromMetaSnapshotJSONL(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-recover"
	dir := paths.SessionDir(sid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UnixNano()
	events := []Event{
		mkEvent(1, sid, EventUserMessage, now),
		mkEvent(2, sid, EventAssistantMessage, now+1),
		mkEvent(7, sid, EventToolCall, now+2),
	}
	writeEventsTo(t, paths.EventsFile(sid), events)
	got, err := RecoverSeq(paths, sid)
	if err != nil {
		t.Fatal(err)
	}
	if got != 7 {
		t.Fatalf("recover from jsonl only: %d", got)
	}

	snap := SessionSnapshot{SessionID: sid, LastSeq: 42, CutoffSeq: 42}
	if _, err := WriteSnapshotAtomic(dir, snap, SnapshotJSON, false); err != nil {
		t.Fatal(err)
	}
	got, _ = RecoverSeq(paths, sid)
	if got != 42 {
		t.Fatalf("recover with snapshot 42: %d", got)
	}

	meta := &Meta{SessionID: sid, LastSeq: 99, EventCount: 99}
	if err := WriteMetaAtomic(dir, meta, false); err != nil {
		t.Fatal(err)
	}
	got, _ = RecoverSeq(paths, sid)
	if got != 99 {
		t.Fatalf("recover with meta 99: %d", got)
	}
}

func TestSeq_RegistryStableAcrossCalls(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	reg := NewSeqRegistry(paths)
	a1, _ := reg.For("s1")
	a2, _ := reg.For("s1")
	if a1 != a2 {
		t.Fatal("registry must return the same allocator for the same sid")
	}
	a3, _ := reg.For("s2")
	if a3 == a1 {
		t.Fatal("registry must give distinct allocators per session")
	}
	_ = a3
}

func TestSeq_CompactDoneHasUniqueSeqAboveCutoff(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-compact-seq"
	events := setupSession(t, paths, sid, 12)
	state := buildStateFromEvents(events)
	c := NewCompactor(CompactorConfig{Paths: paths})

	res, err := c.Compact(context.Background(), state, CompactOptions{TruncateMode: TruncateSync})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if res.CompactDoneSeq <= res.CutoffSeq {
		t.Fatalf("compact_done seq must be > cutoff: cutoff=%d done=%d", res.CutoffSeq, res.CompactDoneSeq)
	}
	if res.CompactDoneSeq != 13 {
		t.Fatalf("compact_done seq want 13 got %d", res.CompactDoneSeq)
	}

	loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.State.Compactions) != 1 {
		t.Fatalf("expected 1 compaction entry, got %d", len(loaded.State.Compactions))
	}
	entry := loaded.State.Compactions[0]
	if entry.Seq != res.CompactDoneSeq || entry.CutoffSeq != res.CutoffSeq {
		t.Fatalf("compaction entry mismatch: %+v vs (%d,%d)", entry, res.CompactDoneSeq, res.CutoffSeq)
	}
	if entry.SnapshotSHA256 == "" {
		t.Fatal("compaction entry missing snapshot hash")
	}
}

func TestSeq_AllEventsStrictlyMonotonicOnDiskAndState(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-mono"
	dir := paths.SessionDir(sid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	reg := NewSeqRegistry(paths)
	alloc, _ := reg.For(sid)
	state := NewSessionState(sid)
	c := NewCompactor(CompactorConfig{Paths: paths, Seqs: reg})

	w, err := OpenJSONLAppend(paths.EventsFile(sid), false)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	now := time.Now().UnixNano()
	mkAny := func(kind EventType, i int) Event {
		ev := Event{Seq: alloc.Next(), Type: kind, TsUnixNano: now + int64(i), SessionID: sid}
		switch kind {
		case EventUserMessage, EventAssistantMessage:
			ev.Message = &MessagePayload{Role: "user", Content: fmt.Sprintf("%d", i)}
		case EventToolCall:
			ev.Tool = &ToolPayload{CallID: fmt.Sprintf("c%d", i), Name: "x"}
		case EventToolResult:
			ev.Tool = &ToolPayload{CallID: fmt.Sprintf("c%d", i), Status: "completed"}
		case EventError:
			ev.Error = &ErrorPayload{Code: "TEST", Message: fmt.Sprintf("err %d", i)}
		}
		return ev
	}

	types := []EventType{EventUserMessage, EventAssistantMessage, EventToolCall, EventToolResult, EventError}
	for i := 0; i < 50; i++ {
		ev := mkAny(types[i%len(types)], i)
		if _, err := w.Write([]Event{ev}); err != nil {
			t.Fatal(err)
		}
		applyLocked(state, &ev)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Compact(context.Background(), state, CompactOptions{TruncateMode: TruncateDeferred}); err != nil {
		t.Fatal(err)
	}

	for i := 50; i < 70; i++ {
		ev := mkAny(types[i%len(types)], i)
		if _, err := w.Write([]Event{ev}); err != nil {
			t.Fatal(err)
		}
		applyLocked(state, &ev)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	res, err := ReadJSONL(paths.EventsFile(sid), JSONLStrict, "")
	if err != nil {
		t.Fatal(err)
	}

	var seqs []uint64
	var sawError, sawCompact, sawTool bool
	for _, ev := range res.Events {
		seqs = append(seqs, ev.Seq)
		switch ev.Type {
		case EventError:
			sawError = true
		case EventCompactDone:
			sawCompact = true
		case EventToolCall, EventToolResult:
			sawTool = true
		}
	}
	if !sawError || !sawCompact || !sawTool {
		t.Fatalf("expected at least one error/compact/tool event in disk: err=%v compact=%v tool=%v", sawError, sawCompact, sawTool)
	}
	if !sort.SliceIsSorted(seqs, func(i, j int) bool { return seqs[i] < seqs[j] }) {
		t.Fatalf("seqs not sorted on disk: %v", seqs)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] == seqs[i-1] {
			t.Fatalf("duplicate seq %d", seqs[i])
		}
	}
}

func TestSeq_RecoverAfterMultipleCompactions(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-multi"
	events := setupSession(t, paths, sid, 10)
	state := buildStateFromEvents(events)
	c := NewCompactor(CompactorConfig{Paths: paths})

	r1, _ := c.Compact(context.Background(), state, CompactOptions{TruncateMode: TruncateSync})

	alloc, _ := c.Seqs().For(sid)
	for i := 0; i < 5; i++ {
		ev := mkEvent(alloc.Next(), sid, EventUserMessage, time.Now().UnixNano()+int64(i))
		writeEventsTo(t, paths.EventsFile(sid), []Event{ev})
		applyLocked(state, &ev)
	}
	r2, _ := c.Compact(context.Background(), state, CompactOptions{TruncateMode: TruncateSync})

	reg2 := NewSeqRegistry(paths)
	a2, _ := reg2.For(sid)
	if a2.Current() < r2.CompactDoneSeq {
		t.Fatalf("recovered seq below last compact_done: cur=%d compactDone=%d", a2.Current(), r2.CompactDoneSeq)
	}
	nxt := a2.Next()
	if nxt <= r2.CompactDoneSeq {
		t.Fatalf("next after recovery did not advance past last seq: %d vs %d", nxt, r2.CompactDoneSeq)
	}
	_ = r1
}

func TestCrossPlatform_FilesystemBasicsWork(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-xplatform"
	events := setupSession(t, paths, sid, 8)
	state := buildStateFromEvents(events)
	c := NewCompactor(CompactorConfig{Paths: paths, Fsync: true})

	res, err := c.Compact(context.Background(), state, CompactOptions{TruncateMode: TruncateSync})
	if err != nil {
		t.Fatalf("compact (os=%s): %v", runtime.GOOS, err)
	}
	if res.SnapshotSHA256 == "" {
		t.Fatal("snapshot hash empty")
	}
	if _, err := os.Stat(paths.SnapshotFile(sid)); err != nil {
		t.Fatalf("snapshot not at expected path (os=%s): %v", runtime.GOOS, err)
	}

	entries, _ := os.ReadDir(paths.SessionDir(sid))
	for _, e := range entries {
		name := e.Name()
		if len(name) > 0 && name[0] == '.' {
			t.Fatalf("leftover tmp file after compaction on %s: %s", runtime.GOOS, name)
		}
	}

	loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
	if err != nil {
		t.Fatalf("load on %s: %v", runtime.GOOS, err)
	}
	if loaded.State.LastSeq != res.CompactDoneSeq {
		t.Fatalf("last_seq on %s: %d vs %d", runtime.GOOS, loaded.State.LastSeq, res.CompactDoneSeq)
	}
}

func TestCrossPlatform_ConcurrentWritesUnderLoad(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	c := NewCompactor(CompactorConfig{Paths: paths, MaxConcurrent: 8})

	const sessions = 24
	const eventsPerSession = 200

	var wg sync.WaitGroup
	errs := make(chan error, sessions)

	for i := 0; i < sessions; i++ {
		wg.Add(1)
		sid := fmt.Sprintf("sess-load-%02d", i)
		go func(sid string) {
			defer wg.Done()
			dir := paths.SessionDir(sid)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				errs <- err
				return
			}
			reg := c.Seqs()
			alloc, err := reg.For(sid)
			if err != nil {
				errs <- err
				return
			}
			state := NewSessionState(sid)
			w, err := OpenJSONLAppend(paths.EventsFile(sid), false)
			if err != nil {
				errs <- err
				return
			}
			for j := 0; j < eventsPerSession; j++ {
				ev := mkEvent(alloc.Next(), sid, EventUserMessage, time.Now().UnixNano())
				if _, err := w.Write([]Event{ev}); err != nil {
					errs <- err
					_ = w.Close()
					return
				}
				applyLocked(state, &ev)
			}
			if err := w.Close(); err != nil {
				errs <- err
				return
			}
			if _, err := c.Compact(context.Background(), state, CompactOptions{TruncateMode: TruncateSync}); err != nil {
				errs <- err
				return
			}
		}(sid)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	for i := 0; i < sessions; i++ {
		sid := fmt.Sprintf("sess-load-%02d", i)
		loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
		if err != nil {
			t.Errorf("%s load: %v", sid, err)
			continue
		}
		if loaded.State.LastSeq < uint64(eventsPerSession) {
			t.Errorf("%s last_seq too low: %d", sid, loaded.State.LastSeq)
		}
	}
}
