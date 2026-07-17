package sessionstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrAlreadyCompacting = errors.New("compaction: already running for this session")
	ErrNothingToCompact  = errors.New("compaction: nothing to compact (no events past cutoff)")
)

type CompactOptions struct {
	BinaryThresholdBytes int64
	ForceBinary          bool
	Fsync                bool
	TruncateMode         TruncateMode
	GlobalLimiter        *Limiter
	Now                  func() time.Time
	Gate                 SessionGate
}

type SessionGate interface {
	LockSession(sid string) func()
	DropFD(sid string)
	FlushPending(ctx context.Context) error
}

type TruncateMode int

const (
	TruncateAsync TruncateMode = iota
	TruncateSync
	TruncateDeferred
)

type CompactResult struct {
	SessionID         string
	CutoffSeq         uint64
	CompactDoneSeq    uint64
	SnapshotBytes     int
	SnapshotSHA256    string
	SnapshotFormat    SnapshotFormat
	EventsCompacted   int
	BytesReclaimed    int64
	JSONLBytesBefore  int64
	JSONLBytesAfter   int64
	CaptureDurationNs int64
	WriteDurationNs   int64
	TruncateDuration  int64
	Truncated         bool
	StartedAtUnixNano int64
	EndedAtUnixNano   int64
}

type Compactor struct {
	paths       Paths
	locks       sync.Map
	limiter     *Limiter
	bytesThresh int64
	forceBinary bool
	fsync       bool
	now         func() time.Time
	seqs        *SeqRegistry
}

type CompactorConfig struct {
	Paths                Paths
	MaxConcurrent        int
	BinaryThresholdBytes int64
	ForceBinary          bool
	Fsync                bool
	Now                  func() time.Time
	Seqs                 *SeqRegistry
}

func NewCompactor(cfg CompactorConfig) *Compactor {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 64
	}
	if cfg.BinaryThresholdBytes <= 0 {
		cfg.BinaryThresholdBytes = defaultBinarySnapshotThreshold
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Seqs == nil {
		cfg.Seqs = NewSeqRegistry(cfg.Paths)
	}
	return &Compactor{
		paths:       cfg.Paths,
		limiter:     NewLimiter(cfg.MaxConcurrent),
		bytesThresh: cfg.BinaryThresholdBytes,
		forceBinary: cfg.ForceBinary,
		fsync:       cfg.Fsync,
		now:         cfg.Now,
		seqs:        cfg.Seqs,
	}
}

func (c *Compactor) Seqs() *SeqRegistry { return c.seqs }

func (c *Compactor) Compact(ctx context.Context, state *SessionState, opts CompactOptions) (*CompactResult, error) {
	if state == nil || state.SessionID == "" {
		return nil, errors.New("compaction: nil state or empty session id")
	}
	if opts.Now == nil {
		opts.Now = c.now
	}
	bytesThresh := opts.BinaryThresholdBytes
	if bytesThresh <= 0 {
		bytesThresh = c.bytesThresh
	}
	limiter := opts.GlobalLimiter
	if limiter == nil {
		limiter = c.limiter
	}

	lockI, _ := c.locks.LoadOrStore(state.SessionID, new(int32))
	lock := lockI.(*int32)
	if !atomic.CompareAndSwapInt32(lock, 0, 1) {
		return nil, ErrAlreadyCompacting
	}
	defer atomic.StoreInt32(lock, 0)

	if err := limiter.Acquire(ctx); err != nil {
		return nil, fmt.Errorf("compaction acquire: %w", err)
	}
	defer limiter.Release()

	dir := c.paths.SessionDir(state.SessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("compaction mkdir: %w", err)
	}

	startedAt := opts.Now().UnixNano()

	captureStart := time.Now()
	snap := state.Snapshot()
	if snap.LastSeq == 0 {
		return nil, ErrNothingToCompact
	}
	cutoff := snap.LastSeq
	snap.CutoffSeq = cutoff
	captureDur := time.Since(captureStart).Nanoseconds()

	prevSnap, _, _ := ReadSnapshot(dir)
	if prevSnap != nil && prevSnap.CutoffSeq >= cutoff {
		return nil, ErrNothingToCompact
	}

	jsonlPath := c.paths.EventsFile(state.SessionID)
	beforeSize := fileSizeOrZero(jsonlPath)

	if jres, jerr := ReadJSONL(jsonlPath, JSONLBestEffort, ""); jerr == nil {
		snap.Messages = MergeMessagesBySeq(snap.Messages, TranscriptFromParts(prevSnap, jres.Events))
	}

	format := SnapshotJSON
	if c.forceBinary || opts.ForceBinary {
		format = SnapshotBinary
	}
	if format == SnapshotJSON {
		if estimateSnapshotSize(&snap) > bytesThresh {
			format = SnapshotBinary
		}
	}

	writeStart := time.Now()
	writeRes, err := WriteSnapshotAtomic(dir, snap, format, opts.Fsync || c.fsync)
	if err != nil {
		return nil, fmt.Errorf("compaction write snapshot: %w", err)
	}
	writeDur := time.Since(writeStart).Nanoseconds()

	if err := updateMetaAfterSnapshot(dir, &snap, writeRes, cutoff); err != nil {
	}

	res := &CompactResult{
		SessionID:         state.SessionID,
		CutoffSeq:         cutoff,
		SnapshotBytes:     writeRes.Bytes,
		SnapshotSHA256:    writeRes.SHA256,
		SnapshotFormat:    writeRes.Format,
		EventsCompacted:   int(snap.EventCount),
		JSONLBytesBefore:  beforeSize,
		CaptureDurationNs: captureDur,
		WriteDurationNs:   writeDur,
		StartedAtUnixNano: startedAt,
	}

	alloc, err := c.seqs.For(state.SessionID)
	if err != nil {
		return res, fmt.Errorf("compaction seq recover: %w", err)
	}
	alloc.Bump(cutoff)
	compactSeq := alloc.Next()
	endedNano := opts.Now().UnixNano()

	compactEvent := Event{
		Seq:        compactSeq,
		Type:       EventCompactDone,
		TsUnixNano: endedNano,
		SessionID:  state.SessionID,
		AppID:      state.AppID,
		UserID:     state.UserID,
		Compact: &CompactPayload{
			CutoffSeq:      cutoff,
			SnapshotSHA256: writeRes.SHA256,
			Binary:         writeRes.Format == SnapshotBinary,
			EventsBefore:   int(snap.EventCount),
			BytesBefore:    beforeSize,
			DurationMs:     (endedNano - startedAt) / int64(time.Millisecond),
		},
	}

	jw, err := OpenJSONLAppend(jsonlPath, opts.Fsync || c.fsync)
	if err != nil {
		return res, fmt.Errorf("compaction open jsonl: %w", err)
	}
	if _, err := jw.Write([]Event{compactEvent}); err != nil {
		_ = jw.Close()
		return res, fmt.Errorf("compaction append compact_done: %w", err)
	}
	if err := jw.Close(); err != nil {
		return res, fmt.Errorf("compaction close jsonl: %w", err)
	}

	Apply(state, &compactEvent)
	res.CompactDoneSeq = compactSeq

	trMode := opts.TruncateMode
	switch trMode {
	case TruncateSync:
		tDur, afterSize, trErr := truncateUnderGate(opts.Gate, state.SessionID, jsonlPath, cutoff, opts.Fsync || c.fsync)
		res.TruncateDuration = tDur.Nanoseconds()
		res.JSONLBytesAfter = afterSize
		res.BytesReclaimed = beforeSize - afterSize
		res.Truncated = trErr == nil
		if trErr != nil {
			return res, fmt.Errorf("compaction truncate: %w", trErr)
		}
	case TruncateAsync:
		gate := opts.Gate
		sid := state.SessionID
		fs := opts.Fsync || c.fsync
		go func() {
			_, _, _ = truncateUnderGate(gate, sid, jsonlPath, cutoff, fs)
		}()
	case TruncateDeferred:
	}

	res.EndedAtUnixNano = endedNano
	return res, nil
}

func updateMetaAfterSnapshot(dir string, snap *SessionSnapshot, wr *WriteSnapshotResult, cutoff uint64) error {
	metaPath := filepath.Join(dir, metaFilename)
	existing, _ := ReadMeta(metaPath)
	if existing == nil {
		existing = &Meta{}
	}
	existing.SessionID = snap.SessionID
	if snap.AppID != "" {
		existing.AppID = snap.AppID
	}
	if snap.UserID != "" {
		existing.UserID = snap.UserID
	}
	if existing.FirstSeq == 0 || snap.FirstSeq < existing.FirstSeq {
		existing.FirstSeq = snap.FirstSeq
	}
	if snap.LastSeq > existing.LastSeq {
		existing.LastSeq = snap.LastSeq
	}
	if snap.EventCount > existing.EventCount {
		existing.EventCount = snap.EventCount
	}
	if existing.StartedAtNano == 0 {
		existing.StartedAtNano = snap.StartedAtNano
	}
	existing.SnapshotCutoff = cutoff
	existing.SnapshotSHA256 = wr.SHA256
	existing.SnapshotBinary = wr.Format == SnapshotBinary
	if snap.Title != "" {
		existing.Title = snap.Title
	}
	if snap.Workspace != "" {
		existing.Workspace = snap.Workspace
	}
	if snap.Workdir != "" {
		existing.Workdir = snap.Workdir
	}
	if snap.EntryAgent != "" {
		existing.EntryAgent = snap.EntryAgent
	}
	if snap.ContextExtra != "" {
		existing.ContextExtra = snap.ContextExtra
	}
	existing.Partial = snap.Partial
	return WriteMetaAtomic(dir, existing, false)
}

func truncateUnderGate(gate SessionGate, sid, path string, cutoff uint64, fsync bool) (time.Duration, int64, error) {
	if gate != nil {
		unlock := gate.LockSession(sid)
		defer unlock()
		_ = gate.FlushPending(context.Background())
		gate.DropFD(sid)
	}
	return truncateJSONLBeforeCutoff(path, cutoff, fsync)
}

func truncateJSONLBeforeCutoff(path string, cutoff uint64, fsync bool) (time.Duration, int64, error) {
	start := time.Now()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return time.Since(start), 0, fmt.Errorf("truncate stat: %w", err)
	}
	dir := filepath.Dir(path)
	res, err := ReadJSONL(path, JSONLBestEffort, "")
	if err != nil {
		return time.Since(start), 0, fmt.Errorf("truncate read: %w", err)
	}
	keep := res.Events[:0]
	for _, ev := range res.Events {
		if ev.Seq > cutoff {
			keep = append(keep, ev)
		}
	}

	tmp, err := os.CreateTemp(dir, tmpEventsPrefix+"*")
	if err != nil {
		return time.Since(start), 0, fmt.Errorf("truncate tmp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	w := &JSONLWriter{f: tmp, bw: newBufWriter(tmp), fsync: fsync}
	if _, err := w.Write(keep); err != nil {
		w.Close()
		return time.Since(start), 0, fmt.Errorf("truncate write: %w", err)
	}
	if err := w.Flush(); err != nil {
		w.Close()
		return time.Since(start), 0, fmt.Errorf("truncate flush: %w", err)
	}
	if err := w.Close(); err != nil {
		return time.Since(start), 0, fmt.Errorf("truncate close: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return time.Since(start), 0, fmt.Errorf("truncate rename: %w", err)
	}
	cleanup = false
	afterSize := fileSizeOrZero(path)
	return time.Since(start), afterSize, nil
}

func fileSizeOrZero(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

func estimateSnapshotSize(snap *SessionSnapshot) int64 {
	if snap == nil {
		return 0
	}
	var n int64
	n += int64(len(snap.SessionID) + len(snap.AppID) + len(snap.UserID))
	n += int64(len(snap.Title) + len(snap.Workspace) + len(snap.Workdir) + len(snap.Goal))
	for i := range snap.Messages {
		n += int64(len(snap.Messages[i].Content) + len(snap.Messages[i].Role))
		for _, id := range snap.Messages[i].ToolCallIDs {
			n += int64(len(id))
		}
	}
	for _, tc := range snap.ToolCalls {
		n += int64(len(tc.Name) + len(tc.Error))
		for k := range tc.Arguments {
			n += int64(len(k) + 32)
		}
	}
	for _, f := range snap.WorkspaceFiles {
		n += int64(len(f.Path) + len(f.ContentHash))
	}
	for _, fact := range snap.Facts {
		n += int64(len(fact))
	}
	n += int64(256 * (len(snap.ToolCalls) + len(snap.Approvals) + len(snap.WorkspaceFiles) + len(snap.Widgets) + len(snap.Previews)))
	return n
}
