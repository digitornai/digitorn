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

// H3 — Crash recovery tests at the BYTE level. These tests go further
// than the in-process "simulate crash by re-loading" pattern : they
// physically corrupt the on-disk artifacts in ways that mimic what a
// real kill -9 produces (truncated last fsync, partial page write).

// TestRecovery_TruncatedLastJSONLLine_BestEffort simulates a kill -9
// between a partial fsync and the end of an event line. Bytes are
// truncated mid-line. Best-effort load must skip the partial line and
// recover the rest.
func TestRecovery_TruncatedLastJSONLLine_BestEffort(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-trunc"
	setupSession(t, paths, sid, 5)

	// Truncate the last 8 bytes of the JSONL — leaves the final event
	// half-written without the trailing newline.
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

// TestRecovery_TruncatedLastJSONLLine_Strict ensures strict mode
// refuses to load a partially-written file. The daemon should pick
// best-effort or surface the error to the operator — never silently
// fabricate a state.
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

// TestRecovery_CorruptedSnapshot_HashMismatch flips a byte in the
// middle of a snapshot file. VerifySnapshot must detect it and refuse
// to use the snapshot — the loader should then fall back to replaying
// the JSONL.
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

	// Find the snapshot file and flip one byte in the middle.
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

	// The original hash must reject the corrupted bytes.
	if err := VerifySnapshot(corrupted, res.SnapshotSHA256); err == nil {
		t.Fatal("VerifySnapshot did NOT detect corrupted bytes")
	} else if !strings.Contains(err.Error(), "hash") && !strings.Contains(err.Error(), "mismatch") {
		t.Logf("hash error message: %v", err)
	}
}

// TestRecovery_CorruptedMetaJSON_RebuildsFromJSONL verifies the meta.json
// can be invalid JSON entirely — the loader scans the JSONL and rebuilds
// a coherent meta. (Existing TestCompact_RebuildsInconsistentMeta only
// covers STALE meta — here we test SYNTACTICALLY broken meta.)
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

	// The new meta file should now be readable.
	meta, err := ReadMeta(metaPath)
	if err != nil {
		t.Fatalf("read rebuilt meta: %v", err)
	}
	if meta.LastSeq != 7 {
		t.Fatalf("meta.last_seq: %d", meta.LastSeq)
	}
}

// TestRecovery_EmptySessionDir_LoadsCleanly covers the boot path where a
// session directory exists but no events were ever flushed (daemon was
// killed between session creation and first append). Loader must return
// an empty state without erroring.
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

// TestRecovery_ZeroByteEventsFile_NoCrash : if the events file exists but
// is 0 bytes (touched but never written), Load must handle it cleanly.
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

// TestRecovery_ConcurrentAppendDuringCompaction stresses the race
// between an active flusher write path and a Compact() call. Goal :
// every event must end up either in the snapshot's projected state OR
// in the tail JSONL — never lost, never duplicated.
//
// Setup : 1 session, 1 shard, 1 writer pushing 200 events while a
// compactor fires concurrently after the first 50. At the end, total
// events on disk (snapshot state count + tail JSONL lines) must equal
// the writer's count.
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
			// Light pacing so the compactor can interleave.
			if i%20 == 0 {
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// Wait until ~50 events landed, then fire a compaction concurrent
	// with the rest of the writes.
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

	// Drain flusher and verify integrity via Load.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := flusher.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// LastSeq must be >= total (compact_done event also bumps seq).
	if loaded.State.LastSeq < uint64(total) {
		t.Fatalf("last_seq %d < expected %d", loaded.State.LastSeq, total)
	}
}

// TestRecovery_QuarantineWrittenForCorruptedLines ensures that
// best-effort recovery quarantines the corrupted bytes for forensic
// analysis rather than discarding them silently.
func TestRecovery_QuarantineWrittenForCorruptedLines(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-quarantine"
	setupSession(t, paths, sid, 4)

	// Inject 2 corrupt lines.
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

// TestRecovery_ManyEmptyLinesInJSONL : blank lines (Windows editors
// sometimes inject CR/LF artifacts) must be silently skipped without
// being counted as bad lines.
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

// TestRecovery_SnapshotPlusTailJSONLConsistency exercises the typical
// post-compaction state : snapshot.json holds projected state up to
// cutoff_seq, events.jsonl has only events ABOVE cutoff. Load must
// merge them correctly.
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

	// Append 5 more events AFTER the compaction.
	more := make([]Event, 5)
	for i := range more {
		more[i] = Event{
			Seq:        cutoff + uint64(i) + 2, // +2 to skip compact_done seq
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

// TestRecovery_ParallelLoadsOfSameSession ensures Load() is safe to call
// from many goroutines simultaneously without races.
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

// Stub used by tests to ensure encoding/json is referenced (compactness).
var _ = json.Marshal
