package sessionstore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecovery_TruncatedLastJSONLLine_BestEffort(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-trunc"
	setupSession(t, paths, sid, 5)

	path := paths.EventsFile(sid)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 16 {
		t.Fatalf("events file too small to truncate: %d bytes", info.Size())
	}
	if err := os.Truncate(path, info.Size()-8); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLBestEffort})
	if err != nil {
		t.Fatalf("best-effort load: %v", err)
	}
	if loaded.EventsApplied < 4 {
		t.Fatalf("expected at least 4 events to survive truncation, got %d", loaded.EventsApplied)
	}
	if !loaded.State.Partial {
		t.Fatal("expected Partial flag set after truncation")
	}
	if loaded.BadEventLines < 1 {
		t.Fatalf("expected at least 1 bad line, got %d", loaded.BadEventLines)
	}
}

func TestRecovery_TruncatedLastJSONLLine_Strict(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-trunc-strict"
	setupSession(t, paths, sid, 5)

	path := paths.EventsFile(sid)
	info, _ := os.Stat(path)
	if err := os.Truncate(path, info.Size()-8); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict}); err == nil {
		t.Fatal("strict mode must fail on truncated JSONL")
	}
}

func TestRecovery_CorruptedSnapshot_HashMismatch(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-snap-corrupt"
	events := setupSession(t, paths, sid, 20)
	state := buildStateFromEvents(events)

	c := NewCompactor(CompactorConfig{Paths: paths})
	res, err := c.Compact(context.Background(), state, CompactOptions{TruncateMode: TruncateSync})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if res.CutoffSeq == 0 {
		t.Fatal("nothing compacted")
	}

	snapPath := filepath.Join(paths.SessionDir(sid), "snapshot.json")
	data, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if len(data) < 20 {
		t.Fatalf("snapshot too small to corrupt: %d bytes", len(data))
	}
	corrupted := append([]byte(nil), data...)
	corrupted[len(corrupted)/2] ^= 0xFF
	if err := os.WriteFile(snapPath, corrupted, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := VerifySnapshot(corrupted, res.SnapshotSHA256); err == nil {
		t.Fatal("VerifySnapshot did NOT detect corrupted bytes")
	} else if !strings.Contains(err.Error(), "hash") && !strings.Contains(err.Error(), "mismatch") {
		t.Logf("hash error message: %v", err)
	}
}

func TestRecovery_CorruptedMetaJSON_RebuildsFromJSONL(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-meta-corrupt"
	setupSession(t, paths, sid, 7)

	metaPath := paths.MetaFile(sid)
	if err := os.WriteFile(metaPath, []byte("{this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
	if err != nil {
		t.Fatalf("load (must recover from corrupt meta): %v", err)
	}
	if !loaded.MetaRebuilt {
		t.Fatal("expected MetaRebuilt=true after corrupt meta")
	}
	if loaded.State.LastSeq != 7 {
		t.Fatalf("rebuilt state.last_seq: %d", loaded.State.LastSeq)
	}

	meta, err := ReadMeta(metaPath)
	if err != nil {
		t.Fatalf("read rebuilt meta: %v", err)
	}
	if meta.LastSeq != 7 {
		t.Fatalf("meta.last_seq: %d", meta.LastSeq)
	}
}

func TestRecovery_EmptySessionDir_LoadsCleanly(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-empty"
	if err := os.MkdirAll(paths.SessionDir(sid), 0o755); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
	if err != nil {
		t.Fatalf("load empty dir: %v", err)
	}
	if loaded.EventsApplied != 0 {
		t.Errorf("expected 0 events applied, got %d", loaded.EventsApplied)
	}
	if loaded.State.LastSeq != 0 {
		t.Errorf("expected last_seq=0, got %d", loaded.State.LastSeq)
	}
}

func TestRecovery_ZeroByteEventsFile_NoCrash(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-zero"
	if err := os.MkdirAll(paths.SessionDir(sid), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.EventsFile(sid), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
	if err != nil {
		t.Fatalf("load 0-byte events: %v", err)
	}
	if loaded.EventsApplied != 0 {
		t.Errorf("expected 0 events, got %d", loaded.EventsApplied)
	}
}

func TestRecovery_ConcurrentAppendDuringCompaction(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	flusher, err := NewDiskFlusher(DiskFlusherConfig{
		Paths:            paths,
		NumShards:        1,
		QueueCapPerShard: 4096,
		BatchMax:         32,
		FlushInterval:    2 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	flusher.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		flusher.Stop(ctx)
	}()
	bus, err := NewBus(BusConfig{Paths: paths, Flusher: flusher})
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer bus.Stop(context.Background())

	sid := "sess-race"
	const total = 200
	var (
		writerDone = make(chan struct{})
		appended   atomic.Int64
		writeErr   atomic.Value
	)

	go func() {
		defer close(writerDone)
		for i := 0; i < total; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, err := bus.AppendBlocking(ctx, Event{
				Type: EventUserMessage, SessionID: sid,
				Message: &MessagePayload{Role: "user", Content: "msg"},
			})
			cancel()
			if err != nil {
				writeErr.Store(err)
				return
			}
			appended.Add(1)
			if i%20 == 0 {
				time.Sleep(time.Millisecond)
			}
		}
	}()

	for appended.Load() < 50 {
		time.Sleep(time.Millisecond)
	}
	state, err := bus.State(sid)
	if err != nil || state == nil {
		t.Fatalf("State: %v", err)
	}
	c := bus.Compactor(CompactorConfig{})
	compactDone := make(chan struct{})
	go func() {
		defer close(compactDone)
		_, _ = c.Compact(context.Background(), state, CompactOptions{
			TruncateMode: TruncateDeferred, Gate: bus,
		})
	}()

	<-writerDone
	<-compactDone
	if v := writeErr.Load(); v != nil {
		t.Fatalf("writer error: %v", v)
	}
	if appended.Load() != int64(total) {
		t.Fatalf("appended %d / %d", appended.Load(), total)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := flusher.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.State.LastSeq < uint64(total) {
		t.Fatalf("last_seq %d < expected %d", loaded.State.LastSeq, total)
	}
}

func TestRecovery_QuarantineWrittenForCorruptedLines(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-quarantine"
	setupSession(t, paths, sid, 4)

	f, err := os.OpenFile(paths.EventsFile(sid), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("garbage 1\n")
	f.WriteString("{still bad}\n")
	f.Close()

	loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLBestEffort})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.BadEventLines != 2 {
		t.Fatalf("bad lines: %d", loaded.BadEventLines)
	}

	q := paths.QuarantineFile(sid)
	data, err := os.ReadFile(q)
	if err != nil {
		t.Fatalf("read quarantine: %v", err)
	}
	if !strings.Contains(string(data), "garbage 1") || !strings.Contains(string(data), "still bad") {
		t.Fatalf("quarantine missing original bytes: %s", string(data))
	}
}

func TestRecovery_ManyEmptyLinesInJSONL(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-blanks"
	setupSession(t, paths, sid, 3)

	f, err := os.OpenFile(paths.EventsFile(sid), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("\n\n\n")
	f.Close()

	loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
	if err != nil {
		t.Fatalf("strict load with blank lines: %v", err)
	}
	if loaded.EventsApplied != 3 {
		t.Fatalf("events: %d", loaded.EventsApplied)
	}
}

func TestRecovery_SnapshotPlusTailJSONLConsistency(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-merged"
	events := setupSession(t, paths, sid, 30)
	state := buildStateFromEvents(events)

	c := NewCompactor(CompactorConfig{Paths: paths})
	res, err := c.Compact(context.Background(), state, CompactOptions{TruncateMode: TruncateSync})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	cutoff := res.CutoffSeq
	if cutoff == 0 {
		t.Fatal("no cutoff")
	}

	more := make([]Event, 5)
	for i := range more {
		more[i] = Event{
			Seq:        cutoff + uint64(i) + 2,
			Type:       EventUserMessage,
			SessionID:  sid,
			TsUnixNano: time.Now().UnixNano(),
			Message:    &MessagePayload{Role: "user", Content: "after-compact"},
		}
	}
	writeEventsTo(t, paths.EventsFile(sid), more)

	loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
	if err != nil {
		t.Fatalf("merged load: %v", err)
	}
	if !loaded.HadSnapshot {
		t.Fatal("expected HadSnapshot=true")
	}
	if loaded.State.LastSeq < cutoff+1 {
		t.Fatalf("merged last_seq %d < cutoff %d", loaded.State.LastSeq, cutoff)
	}
}

func TestRecovery_ParallelLoadsOfSameSession(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-parallel-load"
	setupSession(t, paths, sid, 50)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
			if err != nil {
				t.Errorf("parallel load: %v", err)
				return
			}
			if loaded.EventsApplied != 50 {
				t.Errorf("events: %d", loaded.EventsApplied)
			}
		}()
	}
	wg.Wait()
}

var _ = json.Marshal
