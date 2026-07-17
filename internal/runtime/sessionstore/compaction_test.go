package sessionstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func mkEvent(seq uint64, sid string, t EventType, ts int64) Event {
	ev := Event{Seq: seq, Type: t, TsUnixNano: ts, SessionID: sid}
	switch t {
	case EventUserMessage, EventAssistantMessage:
		ev.Message = &MessagePayload{
			Role:    map[EventType]string{EventUserMessage: "user", EventAssistantMessage: "assistant"}[t],
			Content: fmt.Sprintf("message #%d", seq),
		}
	case EventToolCall:
		ev.Tool = &ToolPayload{CallID: fmt.Sprintf("call-%d", seq), Name: "read", Status: "pending"}
	case EventToolResult:
		ev.Tool = &ToolPayload{CallID: fmt.Sprintf("call-%d", seq-1), Status: "completed", Output: "ok"}
	}
	return ev
}

func writeEventsTo(t *testing.T, path string, events []Event) {
	t.Helper()
	w, err := OpenJSONLAppend(path, false)
	if err != nil {
		t.Fatalf("open jsonl: %v", err)
	}
	if _, err := w.Write(events); err != nil {
		t.Fatalf("write events: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close jsonl: %v", err)
	}
}

func setupSession(t *testing.T, paths Paths, sid string, n int) []Event {
	t.Helper()
	dir := paths.SessionDir(sid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	now := time.Now().UnixNano()
	events := make([]Event, 0, n)
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
		events = append(events, mkEvent(uint64(i), sid, typ, now+int64(i)))
	}
	writeEventsTo(t, paths.EventsFile(sid), events)
	return events
}

func buildStateFromEvents(events []Event) *SessionState {
	s := NewSessionState(events[0].SessionID)
	for i := range events {
		applyLocked(s, &events[i])
	}
	return s
}

func TestCompact_BasicAtomicSnapshot(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-basic"
	events := setupSession(t, paths, sid, 50)
	state := buildStateFromEvents(events)

	c := NewCompactor(CompactorConfig{Paths: paths, MaxConcurrent: 4})
	res, err := c.Compact(context.Background(), state, CompactOptions{TruncateMode: TruncateSync})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if res.CutoffSeq != 50 {
		t.Fatalf("cutoff: want 50 got %d", res.CutoffSeq)
	}
	if !res.Truncated {
		t.Fatal("expected truncated")
	}
	if res.JSONLBytesAfter >= res.JSONLBytesBefore {
		t.Fatalf("expected reclaim: before=%d after=%d", res.JSONLBytesBefore, res.JSONLBytesAfter)
	}

	loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
	if err != nil {
		t.Fatalf("load after compact: %v", err)
	}
	if !loaded.HadSnapshot {
		t.Fatal("expected snapshot after compact")
	}
	if loaded.SnapshotCutoff != 50 {
		t.Fatalf("cutoff after load: want 50 got %d", loaded.SnapshotCutoff)
	}
	if loaded.State.LastSeq != res.CompactDoneSeq {
		t.Fatalf("last_seq after load: want %d (compact_done) got %d", res.CompactDoneSeq, loaded.State.LastSeq)
	}
	if loaded.EventsApplied != 1 {
		t.Fatalf("expected only compact_done as tail event, got %d", loaded.EventsApplied)
	}
}

func TestCompact_ChainedCompactionsPreserveHistory(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-chain"

	first := setupSession(t, paths, sid, 30)
	state := buildStateFromEvents(first)
	c := NewCompactor(CompactorConfig{Paths: paths})

	res1, err := c.Compact(context.Background(), state, CompactOptions{TruncateMode: TruncateSync})
	if err != nil {
		t.Fatalf("first compact: %v", err)
	}
	if res1.CutoffSeq != 30 {
		t.Fatalf("first cutoff: %d", res1.CutoffSeq)
	}
	if res1.CompactDoneSeq <= res1.CutoffSeq {
		t.Fatalf("compact_done seq must be > cutoff: cutoff=%d done=%d", res1.CutoffSeq, res1.CompactDoneSeq)
	}

	alloc, err := c.Seqs().For(sid)
	if err != nil {
		t.Fatalf("seq allocator: %v", err)
	}
	now := time.Now().UnixNano()
	more := []Event{
		mkEvent(alloc.Next(), sid, EventUserMessage, now+1),
		mkEvent(alloc.Next(), sid, EventAssistantMessage, now+2),
		mkEvent(alloc.Next(), sid, EventToolCall, now+3),
		mkEvent(alloc.Next(), sid, EventToolResult, now+4),
	}
	writeEventsTo(t, paths.EventsFile(sid), more)
	for i := range more {
		applyLocked(state, &more[i])
	}

	res2, err := c.Compact(context.Background(), state, CompactOptions{TruncateMode: TruncateSync})
	if err != nil {
		t.Fatalf("second compact: %v", err)
	}
	if res2.CutoffSeq != more[len(more)-1].Seq {
		t.Fatalf("second cutoff: want %d got %d", more[len(more)-1].Seq, res2.CutoffSeq)
	}
	if res2.CompactDoneSeq <= res1.CompactDoneSeq {
		t.Fatalf("compact_done seq must keep growing across compactions: first=%d second=%d", res1.CompactDoneSeq, res2.CompactDoneSeq)
	}

	loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.State.LastSeq != res2.CompactDoneSeq {
		t.Fatalf("last_seq: want %d got %d", res2.CompactDoneSeq, loaded.State.LastSeq)
	}
	wantMsgs := 0
	for _, e := range first {
		if e.Type == EventUserMessage || e.Type == EventAssistantMessage || e.Type == EventToolResult {
			wantMsgs++
		}
	}
	for _, e := range more {
		if e.Type == EventUserMessage || e.Type == EventAssistantMessage || e.Type == EventToolResult {
			wantMsgs++
		}
	}
	if len(loaded.State.Messages) != wantMsgs {
		t.Fatalf("messages: want %d got %d", wantMsgs, len(loaded.State.Messages))
	}
	if len(loaded.State.Compactions) != 2 {
		t.Fatalf("expected 2 compaction entries in state, got %d", len(loaded.State.Compactions))
	}
}

func TestCompact_NothingToCompactReturnsError(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-empty"
	if err := os.MkdirAll(paths.SessionDir(sid), 0o755); err != nil {
		t.Fatal(err)
	}
	state := NewSessionState(sid)
	c := NewCompactor(CompactorConfig{Paths: paths})
	_, err := c.Compact(context.Background(), state, CompactOptions{})
	if err == nil {
		t.Fatal("expected ErrNothingToCompact")
	}
	if err != ErrNothingToCompact {
		t.Fatalf("expected ErrNothingToCompact got %v", err)
	}
}

func TestCompact_DoubleCallReturnsErrAlreadyCompacting(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-locked"
	events := setupSession(t, paths, sid, 10)
	state := buildStateFromEvents(events)

	limiter := NewLimiter(1)
	if err := limiter.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}

	c := NewCompactor(CompactorConfig{Paths: paths, MaxConcurrent: 1})

	var wg sync.WaitGroup
	wg.Add(1)
	startedFirst := make(chan struct{})
	go func() {
		defer wg.Done()
		close(startedFirst)
		_, _ = c.Compact(context.Background(), state, CompactOptions{
			GlobalLimiter: limiter, TruncateMode: TruncateDeferred,
		})
	}()
	<-startedFirst
	time.Sleep(10 * time.Millisecond)

	_, err := c.Compact(context.Background(), state, CompactOptions{
		GlobalLimiter: limiter, TruncateMode: TruncateDeferred,
	})
	if err != ErrAlreadyCompacting {
		t.Fatalf("want ErrAlreadyCompacting got %v", err)
	}
	limiter.Release()
	wg.Wait()
}

func TestCompact_TruncateDeferredKeepsAllEvents(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-deferred"
	events := setupSession(t, paths, sid, 20)
	state := buildStateFromEvents(events)
	before := fileSizeOrZero(paths.EventsFile(sid))

	c := NewCompactor(CompactorConfig{Paths: paths})
	res, err := c.Compact(context.Background(), state, CompactOptions{TruncateMode: TruncateDeferred})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if res.Truncated {
		t.Fatal("deferred mode should not truncate")
	}
	after := fileSizeOrZero(paths.EventsFile(sid))
	if after <= before {
		t.Fatalf("jsonl should grow by exactly the compact_done event (deferred mode): before=%d after=%d", before, after)
	}

	loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.EventsApplied != 1 {
		t.Fatalf("expected compact_done to be the only applied tail event, got %d", loaded.EventsApplied)
	}
	if loaded.State.LastSeq != res.CompactDoneSeq {
		t.Fatalf("last_seq: want %d got %d", res.CompactDoneSeq, loaded.State.LastSeq)
	}
	if len(loaded.State.Compactions) != 1 {
		t.Fatalf("expected 1 compaction entry, got %d", len(loaded.State.Compactions))
	}
	if loaded.State.Compactions[0].CutoffSeq != 20 {
		t.Fatalf("compaction cutoff in state: %d", loaded.State.Compactions[0].CutoffSeq)
	}
}

func TestCompact_SnapshotIntegrityHash(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-hash"
	events := setupSession(t, paths, sid, 5)
	state := buildStateFromEvents(events)

	c := NewCompactor(CompactorConfig{Paths: paths})
	res, err := c.Compact(context.Background(), state, CompactOptions{TruncateMode: TruncateSync})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if res.SnapshotSHA256 == "" {
		t.Fatal("expected snapshot sha256")
	}
	meta, err := ReadMeta(paths.MetaFile(sid))
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if meta == nil {
		t.Fatal("expected meta")
	}
	if meta.SnapshotSHA256 != res.SnapshotSHA256 {
		t.Fatalf("meta sha mismatch: meta=%s res=%s", meta.SnapshotSHA256, res.SnapshotSHA256)
	}
}

func TestCompact_BinarySnapshotSwitch(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-binary"
	events := setupSession(t, paths, sid, 10)
	state := buildStateFromEvents(events)

	c := NewCompactor(CompactorConfig{Paths: paths, ForceBinary: true})
	res, err := c.Compact(context.Background(), state, CompactOptions{TruncateMode: TruncateSync})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if res.SnapshotFormat != SnapshotBinary {
		t.Fatalf("expected SnapshotBinary, got %v", res.SnapshotFormat)
	}
	if _, err := os.Stat(paths.SnapshotBinFile(sid)); err != nil {
		t.Fatalf("snapshot.bin missing: %v", err)
	}
	if _, err := os.Stat(paths.SnapshotFile(sid)); !os.IsNotExist(err) {
		t.Fatal("snapshot.json should be removed when switching to binary")
	}
	loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.SnapshotFormat != SnapshotBinary {
		t.Fatalf("loaded format: %v", loaded.SnapshotFormat)
	}
	if loaded.State.LastSeq != res.CompactDoneSeq {
		t.Fatalf("last_seq: want %d got %d", res.CompactDoneSeq, loaded.State.LastSeq)
	}
}

func TestCompact_CrashBetweenSnapshotAndTruncate(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-crash"
	events := setupSession(t, paths, sid, 25)
	state := buildStateFromEvents(events)

	c := NewCompactor(CompactorConfig{Paths: paths})
	res, err := c.Compact(context.Background(), state, CompactOptions{TruncateMode: TruncateDeferred})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if res.CutoffSeq != 25 {
		t.Fatalf("cutoff: %d", res.CutoffSeq)
	}

	loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.State.LastSeq != res.CompactDoneSeq {
		t.Fatalf("last_seq: want %d got %d", res.CompactDoneSeq, loaded.State.LastSeq)
	}
	if loaded.EventsApplied != 1 {
		t.Fatalf("expected only compact_done to be applied as tail event, got %d", loaded.EventsApplied)
	}
}

func TestCompact_CrashAfterAppendBeforeSnapshot(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-half"
	events := setupSession(t, paths, sid, 15)
	state := buildStateFromEvents(events)
	_ = state

	loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.HadSnapshot {
		t.Fatal("expected no snapshot")
	}
	if loaded.EventsApplied != 15 {
		t.Fatalf("events applied: %d", loaded.EventsApplied)
	}
	if loaded.State.LastSeq != 15 {
		t.Fatalf("last_seq: %d", loaded.State.LastSeq)
	}
}

func TestCompact_ConcurrentSessions_100Parallel(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	c := NewCompactor(CompactorConfig{Paths: paths, MaxConcurrent: 16})

	const N = 100
	type pair struct {
		sid   string
		state *SessionState
	}
	pairs := make([]pair, 0, N)
	for i := 0; i < N; i++ {
		sid := fmt.Sprintf("sess-%03d", i)
		evs := setupSession(t, paths, sid, 20+i%5)
		pairs = append(pairs, pair{sid: sid, state: buildStateFromEvents(evs)})
	}

	var wg sync.WaitGroup
	errCh := make(chan error, N)
	for _, p := range pairs {
		wg.Add(1)
		go func(p pair) {
			defer wg.Done()
			if _, err := c.Compact(context.Background(), p.state, CompactOptions{TruncateMode: TruncateSync}); err != nil {
				errCh <- fmt.Errorf("%s: %w", p.sid, err)
			}
		}(p)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("%v", err)
	}

	for _, p := range pairs {
		loaded, err := Load(paths, p.sid, LoadOptions{Mode: JSONLStrict})
		if err != nil {
			t.Errorf("load %s: %v", p.sid, err)
			continue
		}
		if !loaded.HadSnapshot {
			t.Errorf("%s: no snapshot", p.sid)
			continue
		}
		if loaded.State.LastSeq == 0 {
			t.Errorf("%s: zero last_seq", p.sid)
		}
	}
}

func TestCompact_CorruptedJSONLBestEffort(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-corrupt"
	events := setupSession(t, paths, sid, 10)
	state := buildStateFromEvents(events)

	f, err := os.OpenFile(paths.EventsFile(sid), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{this is not valid json\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLBestEffort})
	if err != nil {
		t.Fatalf("best-effort load: %v", err)
	}
	if loaded.BadEventLines != 1 {
		t.Fatalf("expected 1 bad line, got %d", loaded.BadEventLines)
	}
	if !loaded.State.Partial {
		t.Fatal("expected Partial flag")
	}
	if _, err := os.Stat(paths.QuarantineFile(sid)); err != nil {
		t.Fatalf("quarantine file missing: %v", err)
	}

	if _, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict}); err == nil {
		t.Fatal("strict mode should reject corrupted JSONL")
	}

	_ = state
}

func TestCompact_StrictModeRejectsCorrupted(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-strict"
	events := setupSession(t, paths, sid, 5)
	state := buildStateFromEvents(events)

	if err := os.WriteFile(paths.EventsFile(sid)+".extra", []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(paths.EventsFile(sid), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("garbage line\n")
	f.Close()

	_, err = Load(paths, sid, LoadOptions{Mode: JSONLStrict})
	if err == nil {
		t.Fatal("strict load must fail on bad JSONL")
	}
	if !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("unexpected err: %v", err)
	}

	_ = state
}

func TestCompact_RebuildsInconsistentMeta(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-meta"
	setupSession(t, paths, sid, 8)

	bogus := &Meta{SessionID: sid, LastSeq: 1, EventCount: 1}
	if err := WriteMetaAtomic(paths.SessionDir(sid), bogus, false); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(paths, sid, LoadOptions{Mode: JSONLStrict})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !loaded.MetaRebuilt {
		t.Fatal("expected meta rebuild")
	}
	meta, err := ReadMeta(paths.MetaFile(sid))
	if err != nil {
		t.Fatal(err)
	}
	if meta.LastSeq != 8 || meta.EventCount != 8 {
		t.Fatalf("rebuilt meta wrong: %+v", meta)
	}
}

func TestSnapshot_AtomicReplacement(t *testing.T) {
	tmp := t.TempDir()
	sid := "sid"
	dir := filepath.Join(tmp, sid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	snap := SessionSnapshot{Version: 1, SessionID: sid, LastSeq: 5, CutoffSeq: 5}
	res, err := WriteSnapshotAtomic(dir, snap, SnapshotJSON, false)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpSnapshotPrefix) {
			t.Fatalf("tmp file leaked: %s", e.Name())
		}
	}
	data, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	var got SessionSnapshot
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.CutoffSeq != 5 {
		t.Fatalf("cutoff: %d", got.CutoffSeq)
	}
	if err := VerifySnapshot(data, res.SHA256); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := VerifySnapshot(data, strings.Repeat("0", 64)); err == nil {
		t.Fatal("expected hash mismatch error")
	}
}

func TestCompact_HighThroughput_1000Events(t *testing.T) {
	tmp := t.TempDir()
	paths := NewPaths(tmp)
	sid := "sess-perf"
	events := setupSession(t, paths, sid, 1000)
	state := buildStateFromEvents(events)

	c := NewCompactor(CompactorConfig{Paths: paths})
	start := time.Now()
	res, err := c.Compact(context.Background(), state, CompactOptions{TruncateMode: TruncateSync})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("compact 1000: %v", err)
	}
	if res.EventsCompacted != 1000 {
		t.Fatalf("compacted: %d", res.EventsCompacted)
	}
	t.Logf("1000-event compaction: %v (capture=%dns write=%dns truncate=%dns reclaim=%dKB)",
		elapsed, res.CaptureDurationNs, res.WriteDurationNs, res.TruncateDuration,
		res.BytesReclaimed/1024)
	if elapsed > 2*time.Second {
		t.Errorf("compaction too slow: %v", elapsed)
	}
}
