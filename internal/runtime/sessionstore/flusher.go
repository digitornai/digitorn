package sessionstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type DiskFlusherConfig struct {
	Paths            Paths
	NumShards        int
	QueueCapPerShard int
	BatchMax         int
	FlushInterval    time.Duration
	Fsync            bool
	FDCachePerShard  int
	PerSidQuotaPct   int
	OnWriteError     func(err error, sid string)
}

type DiskFlusher struct {
	cfg     DiskFlusherConfig
	shards  []*shard
	mu      sync.RWMutex
	started bool
	stopped bool
}

func NewDiskFlusher(cfg DiskFlusherConfig) (*DiskFlusher, error) {
	if cfg.Paths.Root == "" {
		return nil, errors.New("flusher: empty paths root")
	}
	if cfg.NumShards <= 0 {
		cfg.NumShards = 32
	}
	if cfg.QueueCapPerShard <= 0 {
		cfg.QueueCapPerShard = 16384
	}
	if cfg.BatchMax <= 0 {
		cfg.BatchMax = 500
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 25 * time.Millisecond
	}
	if cfg.FDCachePerShard <= 0 {
		cfg.FDCachePerShard = 512
	}
	if cfg.PerSidQuotaPct <= 0 {
		cfg.PerSidQuotaPct = 12
	}

	f := &DiskFlusher{cfg: cfg, shards: make([]*shard, cfg.NumShards)}
	for i := 0; i < cfg.NumShards; i++ {
		f.shards[i] = newShard(shardConfig{
			id:              i,
			paths:           cfg.Paths,
			queueCap:        cfg.QueueCapPerShard,
			batchMax:        cfg.BatchMax,
			flushInterval:   cfg.FlushInterval,
			fsync:           cfg.Fsync,
			fdCacheCap:      cfg.FDCachePerShard,
			perSidQuotaPct:  cfg.PerSidQuotaPct,
			writeErrHandler: cfg.OnWriteError,
		})
	}
	return f, nil
}

func (f *DiskFlusher) Start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped {
		return errors.New("flusher: already stopped")
	}
	if f.started {
		return nil
	}
	for _, s := range f.shards {
		s.start()
	}
	f.started = true
	return nil
}

func (f *DiskFlusher) Stop(ctx context.Context) error {
	f.mu.Lock()
	if f.stopped {
		f.mu.Unlock()
		return nil
	}
	if !f.started {
		f.stopped = true
		f.mu.Unlock()
		return nil
	}
	f.stopped = true
	f.mu.Unlock()

	errs := make([]error, 0)
	var wg sync.WaitGroup
	mu := sync.Mutex{}
	for i := range f.shards {
		wg.Add(1)
		s := f.shards[i]
		go func() {
			defer wg.Done()
			if err := s.stopAndDrain(ctx); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("shard %d: %w", s.cfg.id, err))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (f *DiskFlusher) Enqueue(ev Event) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if !f.started || f.stopped {
		return ErrFlusherStop
	}
	idx := ShardOf(ev.SessionID, len(f.shards))
	return f.shards[idx].tryEnqueue(ev)
}

// EnqueueBlocking waits for queue space, respecting ctx. Use this when
// the caller cannot tolerate event drops (REST handlers, Socket.IO
// bridge). Returns ctx.Err() if ctx is cancelled, ErrQueueFull only if
// the per-sid quota gate trips (single session flooding).
func (f *DiskFlusher) EnqueueBlocking(ctx context.Context, ev Event) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if !f.started || f.stopped {
		return ErrFlusherStop
	}
	idx := ShardOf(ev.SessionID, len(f.shards))
	return f.shards[idx].tryEnqueueBlocking(ctx, ev)
}

// EnqueueDurable waits for queue space AND for the event to be fsynced
// to disk before returning. This is the event-sourcing durability
// guarantee : when this call returns nil, the event survives kill -9.
//
// Latency : queue wait + (flush interval ÷ 2) + fsync. Throughput :
// unaffected (events still batch ; only the ack arrives later).
//
// Requires the flusher to be configured with Fsync=true ; otherwise
// "durable" only means "written to OS page cache", which can still be
// lost on power failure but survives a process kill.
func (f *DiskFlusher) EnqueueDurable(ctx context.Context, ev Event) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if !f.started || f.stopped {
		return ErrFlusherStop
	}
	idx := ShardOf(ev.SessionID, len(f.shards))
	return f.shards[idx].tryEnqueueDurable(ctx, ev)
}

// EnqueueDurableBatch is the group-commit form of EnqueueDurable : it
// enqueues every event and waits for all their fsync acks together. All
// events must share one session_id so they route to a single shard and
// batch into one fsync. Returns one result per event (nil = durable).
func (f *DiskFlusher) EnqueueDurableBatch(ctx context.Context, evs []Event) []error {
	res := make([]error, len(evs))
	if len(evs) == 0 {
		return res
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if !f.started || f.stopped {
		for k := range res {
			res[k] = ErrFlusherStop
		}
		return res
	}
	idx := ShardOf(evs[0].SessionID, len(f.shards))
	return f.shards[idx].tryEnqueueDurableBatch(ctx, evs)
}

func (f *DiskFlusher) Flush(ctx context.Context) error {
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok {
		deadline = d
	}
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		all := true
		for _, s := range f.shards {
			if s.queued.Load() > 0 || s.inFlight.Load() > 0 || len(s.queue) > 0 {
				all = false
				break
			}
		}
		if all {
			return nil
		}
		select {
		case <-tick.C:
			if time.Now().After(deadline) {
				return context.DeadlineExceeded
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (f *DiskFlusher) DropFD(sid string) {
	idx := ShardOf(sid, len(f.shards))
	path := f.cfg.Paths.EventsFile(sid)
	f.shards[idx].cache.Drop(path)
}

func (f *DiskFlusher) NumShards() int { return len(f.shards) }

type FlusherStats struct {
	Shards        []ShardStats
	TotalWritten  uint64
	TotalDropped  uint64
	TotalBatches  uint64
	TotalQueued   int64
	TotalFDCached int
}

func (f *DiskFlusher) Stats() FlusherStats {
	out := FlusherStats{Shards: make([]ShardStats, 0, len(f.shards))}
	for _, s := range f.shards {
		st := s.stats()
		out.Shards = append(out.Shards, st)
		out.TotalWritten += st.Written
		out.TotalDropped += st.Dropped
		out.TotalBatches += st.Batches
		out.TotalQueued += st.Queued
		out.TotalFDCached += st.FDCached
	}
	return out
}
