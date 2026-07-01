package sessionstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitornai/digitorn/internal/safego"
)

type encodeBuf struct {
	buf *bytes.Buffer
	enc *json.Encoder
}

var encoderPool = sync.Pool{
	New: func() any {
		b := &bytes.Buffer{}
		b.Grow(512)
		e := json.NewEncoder(b)
		e.SetEscapeHTML(false)
		return &encodeBuf{buf: b, enc: e}
	},
}

var (
	ErrQueueFull   = errors.New("flusher: shard queue full")
	ErrFlusherStop = errors.New("flusher: stopped")
)

type shardConfig struct {
	id              int
	paths           Paths
	queueCap        int
	batchMax        int
	flushInterval   time.Duration
	fsync           bool
	fdCacheCap      int
	perSidQuotaPct  int
	writeErrHandler func(err error, sid string)
}

// queuedEvent wraps an Event in transit through the flusher with an
// optional ack channel. When ack is non-nil, the shard signals it
// (with the write error or nil) AFTER the batch containing this event
// has been fsynced to disk. Used by AppendDurable callers ; the
// fire-and-forget Append path leaves ack nil.
type queuedEvent struct {
	ev  Event
	ack chan error
}

type shard struct {
	cfg shardConfig

	queue chan queuedEvent
	stop  chan struct{}
	done  chan struct{}

	cache *fdCache

	mu          sync.Mutex
	sidPending  map[string]int
	perSidQuota int

	workerGrouped map[string]*sidBuf
	workerScratch []byte

	written  atomic.Uint64
	dropped  atomic.Uint64
	batches  atomic.Uint64
	lastSize atomic.Int64
	queued   atomic.Int64
	inFlight atomic.Int64
	started  atomic.Bool
}

func newShard(cfg shardConfig) *shard {
	if cfg.queueCap <= 0 {
		cfg.queueCap = 16384
	}
	if cfg.batchMax <= 0 {
		cfg.batchMax = 500
	}
	if cfg.flushInterval <= 0 {
		cfg.flushInterval = 25 * time.Millisecond
	}
	if cfg.fdCacheCap <= 0 {
		cfg.fdCacheCap = 512
	}
	if cfg.perSidQuotaPct <= 0 || cfg.perSidQuotaPct > 100 {
		cfg.perSidQuotaPct = 12
	}
	s := &shard{
		cfg:           cfg,
		queue:         make(chan queuedEvent, cfg.queueCap),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
		cache:         newFDCache(cfg.fdCacheCap),
		sidPending:    make(map[string]int, 4096),
		workerGrouped: make(map[string]*sidBuf, 16),
		workerScratch: make([]byte, 0, 64*1024),
	}
	s.perSidQuota = max(1, cfg.queueCap*cfg.perSidQuotaPct/100)
	return s
}

func (s *shard) start() {
	if !s.started.CompareAndSwap(false, true) {
		return
	}
	go s.run()
}

func (s *shard) stopAndDrain(ctx context.Context) error {
	if !s.started.Load() {
		return nil
	}
	close(s.stop)
	select {
	case <-s.done:
		s.cache.Close()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *shard) tryEnqueue(ev Event) error {
	if !s.started.Load() {
		return ErrFlusherStop
	}
	sid := ev.SessionID
	if sid == "" {
		return errors.New("flusher: empty session_id")
	}

	s.mu.Lock()
	pending := s.sidPending[sid]
	if pending >= s.perSidQuota && len(s.queue) > cap(s.queue)/2 {
		s.mu.Unlock()
		s.dropped.Add(1)
		return ErrQueueFull
	}
	s.mu.Unlock()

	select {
	case s.queue <- queuedEvent{ev: ev}:
		s.mu.Lock()
		s.sidPending[sid]++
		s.mu.Unlock()
		s.queued.Add(1)
		return nil
	default:
		s.dropped.Add(1)
		return ErrQueueFull
	}
}

// tryEnqueueBlocking waits for queue space, respecting ctx. Returns
// ErrQueueFull only if ctx is cancelled before space frees up. Used by
// REST/Socket.IO handlers where event loss is unacceptable. The per-sid
// quota gate still applies — a single session cannot starve others.
func (s *shard) tryEnqueueBlocking(ctx context.Context, ev Event) error {
	if !s.started.Load() {
		return ErrFlusherStop
	}
	sid := ev.SessionID
	if sid == "" {
		return errors.New("flusher: empty session_id")
	}

	s.mu.Lock()
	pending := s.sidPending[sid]
	if pending >= s.perSidQuota && len(s.queue) > cap(s.queue)/2 {
		s.mu.Unlock()
		s.dropped.Add(1)
		return ErrQueueFull
	}
	s.mu.Unlock()

	select {
	case s.queue <- queuedEvent{ev: ev}:
		s.mu.Lock()
		s.sidPending[sid]++
		s.mu.Unlock()
		s.queued.Add(1)
		return nil
	case <-ctx.Done():
		s.dropped.Add(1)
		return ctx.Err()
	case <-s.stop:
		return ErrFlusherStop
	}
}

// tryEnqueueDurable is like tryEnqueueBlocking but waits for the
// event to be fsynced to disk before returning. Implements the
// event-sourcing durability contract : when this call returns nil,
// the event survives kill -9. Per-event ack via a 1-slot channel
// signalled by the batch processor after fsync.
//
// Latency is bounded by the next flush interval (typically a few ms)
// plus disk fsync time. Throughput is unaffected — events still
// batch normally ; ack just fires AFTER the batch's fsync.
func (s *shard) tryEnqueueDurable(ctx context.Context, ev Event) error {
	if !s.started.Load() {
		return ErrFlusherStop
	}
	sid := ev.SessionID
	if sid == "" {
		return errors.New("flusher: empty session_id")
	}

	s.mu.Lock()
	pending := s.sidPending[sid]
	if pending >= s.perSidQuota && len(s.queue) > cap(s.queue)/2 {
		s.mu.Unlock()
		s.dropped.Add(1)
		return ErrQueueFull
	}
	s.mu.Unlock()

	ack := make(chan error, 1)
	entry := queuedEvent{ev: ev, ack: ack}

	select {
	case s.queue <- entry:
		s.mu.Lock()
		s.sidPending[sid]++
		s.mu.Unlock()
		s.queued.Add(1)
	case <-ctx.Done():
		s.dropped.Add(1)
		return ctx.Err()
	case <-s.stop:
		return ErrFlusherStop
	}

	// Wait for fsync ack from the batch processor.
	select {
	case err := <-ack:
		return err
	case <-ctx.Done():
		// The event WILL still be flushed by the batch processor — we
		// just don't wait. Caller knows the durability promise broke
		// for this specific call.
		return ctx.Err()
	}
}

// tryEnqueueDurableBatch enqueues every event THEN waits for all their
// fsync acks — the group-commit path. All events must belong to one sid
// (the caller guarantees it), so they land in the same shard and, being
// enqueued back-to-back, ride the same write batch : one fsync, N acks,
// instead of N sequential fsyncs. Returns one result per event : nil when
// that event is durably on disk, else the error. Enqueue failures form a
// clean suffix (we stop at the first), so the committed events are always
// a contiguous prefix.
func (s *shard) tryEnqueueDurableBatch(ctx context.Context, evs []Event) []error {
	res := make([]error, len(evs))
	if len(evs) == 0 {
		return res
	}
	if !s.started.Load() {
		for k := range res {
			res[k] = ErrFlusherStop
		}
		return res
	}
	sid := evs[0].SessionID
	if sid == "" {
		for k := range res {
			res[k] = errors.New("flusher: empty session_id")
		}
		return res
	}

	s.mu.Lock()
	pending := s.sidPending[sid]
	if pending >= s.perSidQuota && len(s.queue) > cap(s.queue)/2 {
		s.mu.Unlock()
		s.dropped.Add(uint64(len(evs)))
		for k := range res {
			res[k] = ErrQueueFull
		}
		return res
	}
	s.mu.Unlock()

	acks := make([]chan error, len(evs))
	var failErr error
	for k := range evs {
		if failErr != nil {
			res[k] = failErr
			s.dropped.Add(1)
			continue
		}
		ack := make(chan error, 1)
		select {
		case s.queue <- queuedEvent{ev: evs[k], ack: ack}:
			s.mu.Lock()
			s.sidPending[sid]++
			s.mu.Unlock()
			s.queued.Add(1)
			acks[k] = ack
		case <-ctx.Done():
			failErr = ctx.Err()
			res[k] = failErr
		case <-s.stop:
			failErr = ErrFlusherStop
			res[k] = failErr
		default:
			s.dropped.Add(1)
			failErr = ErrQueueFull
			res[k] = failErr
		}
	}

	for k := range acks {
		if acks[k] == nil {
			continue
		}
		select {
		case err := <-acks[k]:
			res[k] = err
		case <-ctx.Done():
			res[k] = ctx.Err()
		}
	}
	return res
}

func (s *shard) decPending(sid string) {
	s.mu.Lock()
	cur := s.sidPending[sid]
	if cur <= 1 {
		delete(s.sidPending, sid)
	} else {
		s.sidPending[sid] = cur - 1
	}
	s.mu.Unlock()
}

func (s *shard) run() {
	defer close(s.done)

	ticker := time.NewTicker(s.cfg.flushInterval)
	defer ticker.Stop()

	batch := make([]queuedEvent, 0, s.cfg.batchMax)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.writeBatch(batch)
		s.inFlight.Add(-int64(len(batch)))
		batch = batch[:0]
	}

	addToBatch := func(qe queuedEvent) {
		batch = append(batch, qe)
		s.queued.Add(-1)
		s.inFlight.Add(1)
		s.decPending(qe.ev.SessionID)
	}

	drainPending := func() {
		for {
			select {
			case qe := <-s.queue:
				addToBatch(qe)
				if len(batch) >= s.cfg.batchMax {
					flush()
				}
			default:
				return
			}
		}
	}

	for {
		stop := func() (stop bool) {
			// Per-iteration shield : a panic in writeBatch/flush (a poison event,
			// a disk fault) is recovered and the in-progress batch dropped, so the
			// write-behind loop keeps running instead of crashing the daemon or
			// re-panicking forever on the same batch.
			defer func() {
				if r := recover(); r != nil {
					safego.Report("sessionstore.shard.run", r)
					batch = batch[:0]
				}
			}()
			select {
			case qe := <-s.queue:
				addToBatch(qe)
				if len(batch) >= s.cfg.batchMax {
					flush()
				}
			case <-ticker.C:
				flush()
			case <-s.stop:
				drainPending()
				flush()
				return true
			}
			return false
		}()
		if stop {
			return
		}
	}
}

type sidBuf struct {
	events []Event
	acks   []chan error // parallel to events ; nil entry = the caller didn't wait
	buf    []byte
}

func (s *shard) writeBatch(batch []queuedEvent) {
	if len(batch) == 0 {
		return
	}
	s.batches.Add(1)
	s.lastSize.Store(int64(len(batch)))

	for _, gb := range s.workerGrouped {
		gb.events = gb.events[:0]
		gb.acks = gb.acks[:0]
		gb.buf = gb.buf[:0]
	}

	// Group events by sid, keeping each event's ack channel PAIRED with it (not
	// pooled per-sid). The pairing is what lets a partial encode failure signal
	// exactly the failing event's caller — never falsely ack a sibling as
	// "durable" when its bytes never reached disk.
	for i := range batch {
		qe := &batch[i]
		gb, ok := s.workerGrouped[qe.ev.SessionID]
		if !ok {
			gb = &sidBuf{events: make([]Event, 0, 8), acks: make([]chan error, 0, 8), buf: make([]byte, 0, 4096)}
			s.workerGrouped[qe.ev.SessionID] = gb
		}
		gb.events = append(gb.events, qe.ev)
		gb.acks = append(gb.acks, qe.ack) // nil when the caller didn't wait
	}

	enc := encoderPool.Get().(*encodeBuf)
	defer encoderPool.Put(enc)

	for sid, gb := range s.workerGrouped {
		if len(gb.events) == 0 {
			continue
		}
		sortBySeqWithAcks(gb) // keep each ack paired with its event
		// Encode each event. One that fails to Encode is NOT persisted, so its
		// caller MUST hear the error — never a false "durable" nil because a
		// sibling of the same sid happened to write. Only the successfully
		// encoded events are written and then acked with the write outcome.
		okAcks := make([]chan error, 0, len(gb.acks))
		encoded := 0
		for i := range gb.events {
			enc.buf.Reset()
			if err := enc.enc.Encode(gb.events[i]); err != nil {
				s.reportErr(err, sid)
				s.dropped.Add(1)
				if gb.acks[i] != nil {
					gb.acks[i] <- err // this event never reached disk
				}
				continue
			}
			gb.buf = append(gb.buf, enc.buf.Bytes()...)
			if gb.acks[i] != nil {
				okAcks = append(okAcks, gb.acks[i])
			}
			encoded++
		}
		if len(gb.buf) == 0 {
			continue // nothing encoded ; failing acks already signalled above
		}
		if err := s.writeSession(sid, gb.buf); err != nil {
			s.reportErr(err, sid)
			s.dropped.Add(uint64(encoded))
			signalAcks(okAcks, err)
			continue
		}
		s.written.Add(uint64(encoded))
		// writeSession fdatasync'd when s.cfg.fsync is true — the encoded events
		// for this sid are now durable on disk. Ack the callers that waited.
		signalAcks(okAcks, nil)
	}
}

// signalAcks sends the result (nil or error) to every ack channel
// without blocking. Each channel is buffered with cap 1 so this
// always succeeds.
func signalAcks(acks []chan error, err error) {
	for _, ack := range acks {
		if ack != nil {
			ack <- err
		}
	}
}

func (s *shard) writeSession(sid string, payload []byte) error {
	dir := s.cfg.paths.SessionDir(sid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := s.cfg.paths.EventsFile(sid)
	f, err := s.cache.Get(path)
	if err != nil {
		return err
	}
	if _, err := f.Write(payload); err != nil {
		s.cache.Drop(path)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if s.cfg.fsync {
		if err := fdatasyncFile(f); err != nil {
			return fmt.Errorf("fsync %s: %w", path, err)
		}
	}
	return nil
}

func (s *shard) reportErr(err error, sid string) {
	if s.cfg.writeErrHandler != nil {
		s.cfg.writeErrHandler(err, sid)
	}
}

// sortBySeqWithAcks insertion-sorts a sidBuf by event seq, swapping the parallel
// acks slice in lockstep so every ack channel stays paired with its event after
// the sort (insertion sort : the batches are tiny + nearly-sorted in practice).
func sortBySeqWithAcks(b *sidBuf) {
	for i := 1; i < len(b.events); i++ {
		j := i
		for j > 0 && b.events[j-1].Seq > b.events[j].Seq {
			b.events[j-1], b.events[j] = b.events[j], b.events[j-1]
			b.acks[j-1], b.acks[j] = b.acks[j], b.acks[j-1]
			j--
		}
	}
}

type ShardStats struct {
	ID            int
	Written       uint64
	Dropped       uint64
	Batches       uint64
	LastBatchSize int64
	Queued        int64
	FDCached      int
}

func (s *shard) stats() ShardStats {
	return ShardStats{
		ID:            s.cfg.id,
		Written:       s.written.Load(),
		Dropped:       s.dropped.Load(),
		Batches:       s.batches.Load(),
		LastBatchSize: s.lastSize.Load(),
		Queued:        s.queued.Load(),
		FDCached:      s.cache.Len(),
	}
}
