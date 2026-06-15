package indexer

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Report summarises one Walk sync.
type Report struct {
	Added, Updated, Deleted, Skipped int
}

// Service drives indexation : it runs incremental Walk syncs, schedules
// periodic triggers on a bounded pool, and runs continuous Watch streams.
// One Service is shared by a consumer (e.g. the rag module) across all its
// apps. It never runs on the daemon loop.
type Service struct {
	sched   *scheduler
	cursor  Cursor
	metrics *Metrics

	mu         sync.Mutex
	watches    map[string]context.CancelFunc
	deadLetter func(spec SourceSpec, doc Document, err error)

	poolMu      sync.Mutex
	cursorPools map[string]*PgStore // per-app cursor DSN → shared store

	sfMu    sync.Mutex
	sfLocks map[string]*sync.Mutex // per-source in-process lock : serialises direct Sync of one source

	watchReset time.Duration // a Watch that ran this long before dropping resets its restart backoff
}

// NewService builds a service. maxConcurrent bounds simultaneous syncs across
// ALL apps. A nil cursor uses an in-process MemCursor — the service default,
// used when a source declares no per-app CursorDSN. When a cursor (default or
// per-app) also implements Locker it becomes that source's distributed lease
// backend (only one replica syncs it at a time).
func NewService(cursor Cursor, maxConcurrent int) *Service {
	if cursor == nil {
		cursor = NewMemCursor()
	}
	sched := newScheduler(maxConcurrent, 15*time.Second)
	return &Service{
		sched:       sched,
		cursor:      cursor,
		metrics:     sched.metrics,
		watches:     map[string]context.CancelFunc{},
		cursorPools: map[string]*PgStore{},
		sfLocks:     map[string]*sync.Mutex{},
		watchReset:  30 * time.Second,
	}
}

// cursorFor resolves a source's sync-state store : its own CursorDSN (a store
// in the app's OWN database — index + state + source all client-side, nothing
// local) if set, else the service default. Per-DSN stores are pooled so 10k
// apps sharing a handful of DSNs reuse connections.
func (s *Service) cursorFor(spec SourceSpec) Cursor {
	dsn := strings.TrimSpace(spec.CursorDSN)
	if dsn == "" {
		return s.cursor
	}
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	if c, ok := s.cursorPools[dsn]; ok {
		return c
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := NewPgStore(ctx, dsn)
	if err != nil {
		return s.cursor // unreachable per-app DB → fall back, never block indexing
	}
	s.cursorPools[dsn] = st
	return st
}

// OnDeadLetter registers a handler called for each document that fails to
// upsert even after per-document isolation. The document's cursor hash is not
// advanced, so it retries on the next sync rather than blocking the batch.
func (s *Service) OnDeadLetter(fn func(spec SourceSpec, doc Document, err error)) {
	s.deadLetter = fn
}

// Stats returns a snapshot of the service's runtime counters + live gauges.
func (s *Service) Stats() Stats {
	st := s.metrics.snapshot()
	s.sched.mu.Lock()
	st.Jobs = len(s.sched.jobs)
	s.sched.mu.Unlock()
	s.mu.Lock()
	st.Watches = len(s.watches)
	s.mu.Unlock()
	return st
}

// Shutdown cancels all watch streams and drains in-flight syncs, bounded by
// ctx. After it returns the service runs nothing new.
func (s *Service) Shutdown(ctx context.Context) {
	s.mu.Lock()
	for _, cancel := range s.watches {
		cancel()
	}
	s.watches = map[string]context.CancelFunc{}
	s.mu.Unlock()
	s.sched.shutdown(ctx)

	s.poolMu.Lock()
	for _, st := range s.cursorPools {
		st.Close()
	}
	s.cursorPools = map[string]*PgStore{}
	s.poolMu.Unlock()
}

// syncLock returns the per-source in-process lock, creating it on first use.
func (s *Service) syncLock(key string) *sync.Mutex {
	s.sfMu.Lock()
	defer s.sfMu.Unlock()
	m := s.sfLocks[key]
	if m == nil {
		m = &sync.Mutex{}
		s.sfLocks[key] = m
	}
	return m
}

// Sync runs one incremental Walk of a source into the sink : the connector
// emits the current document set, the service diffs it against the durable
// cursor, and only changed docs are upserted / removed docs deleted.
func (s *Service) Sync(ctx context.Context, spec SourceSpec, sink Sink) (Report, error) {
	s.metrics.inc(&s.metrics.SyncsStarted)
	conn, ok := connectorFor(spec.Type)
	if !ok {
		s.metrics.inc(&s.metrics.SyncsFailed)
		return Report{}, fmt.Errorf("indexer: no connector for type %q", spec.Type)
	}
	if !conn.Capabilities().Walk {
		s.metrics.inc(&s.metrics.SyncsFailed)
		return Report{}, fmt.Errorf("indexer: connector %q has no Walk mode", spec.Type)
	}

	// Serialise concurrent syncs of the SAME source in this process : the second
	// caller blocks until the first has saved the cursor, then sees no changes —
	// so a direct re-sync (manual reindex) never double-embeds. The distributed
	// lease covers the cross-process case ; this covers the in-process one.
	lk := s.syncLock(stateKey(spec))
	lk.Lock()
	defer lk.Unlock()

	present := map[string]string{}
	docs := map[string]Document{}
	if err := conn.Walk(ctx, spec, func(d Document) error {
		if d.ID == "" || strings.TrimSpace(d.Text) == "" {
			return nil
		}
		present[d.ID] = docHash(d)
		docs[d.ID] = d
		return nil
	}); err != nil {
		s.metrics.inc(&s.metrics.SyncsFailed)
		return Report{}, fmt.Errorf("indexer: walk %s: %w", spec.Name, err)
	}

	cur := s.cursorFor(spec)
	key := stateKey(spec)
	prev := s.loadHashes(cur, key)

	var rep Report
	var changed []Document
	for id, h := range present {
		if prev[id] == h {
			continue
		}
		changed = append(changed, docs[id])
		if prev[id] == "" {
			rep.Added++
		} else {
			rep.Updated++
		}
	}

	saved := present
	if len(changed) > 0 {
		dead := s.upsertResilient(ctx, spec, sink, changed)
		for id := range dead {
			delete(saved, id) // not persisted → retried next sync, batch not blocked
		}
	}
	for id := range prev {
		if _, ok := present[id]; !ok {
			if err := sink.Delete(ctx, spec.KB, id); err == nil {
				s.metrics.inc(&s.metrics.DocsDeleted)
				rep.Deleted++
			}
		}
	}
	s.saveHashes(cur, key, saved)
	s.metrics.inc(&s.metrics.SyncsOK)
	s.metrics.stampSync()
	return rep, nil
}

// upsertResilient upserts the batch, and on failure isolates the poison
// document(s) with per-document retries : good docs still land, each doc that
// keeps failing is dead-lettered (counter + hook) and returned so its cursor
// hash is not advanced. A single bad document never blocks a whole sync.
func (s *Service) upsertResilient(ctx context.Context, spec SourceSpec, sink Sink, docs []Document) map[string]bool {
	if err := sink.Upsert(ctx, spec.KB, docs); err == nil {
		s.metrics.add(&s.metrics.DocsUpserted, int64(len(docs)))
		return nil
	}
	dead := map[string]bool{}
	for _, d := range docs {
		if err := sink.Upsert(ctx, spec.KB, []Document{d}); err != nil {
			dead[d.ID] = true
			s.metrics.inc(&s.metrics.DeadLettered)
			if s.deadLetter != nil {
				s.deadLetter(spec, d, err)
			}
			continue
		}
		s.metrics.inc(&s.metrics.DocsUpserted)
	}
	return dead
}

// Register wires a source's triggers : on_start → sync now ; interval/cron →
// scheduled on the bounded pool ; cdc/watch → a long-lived Watch stream.
// Idempotent per source. Deregister stops them (called on engine eviction).
func (s *Service) Register(spec SourceSpec, sink Sink) {
	key := stateKey(spec)
	// The sync closure takes the source's distributed lease (when its cursor is
	// a Locker) before running, so across N replicas only one indexes it.
	syncFn := func(ctx context.Context) error {
		if lk, ok := s.cursorFor(spec).(Locker); ok {
			release, ok := lk.Acquire(ctx, key)
			if !ok {
				s.metrics.inc(&s.metrics.LeaseSkipped)
				return nil
			}
			defer release()
		}
		_, err := s.Sync(ctx, spec, sink)
		return err
	}

	var next func(time.Time) time.Time
	onStart, watch := false, false
	for _, t := range spec.Triggers {
		switch strings.ToLower(t.Type) {
		case "on_start":
			onStart = true
		case "interval":
			if t.Every > 0 {
				next = intervalNext(t.Every)
			}
		case "cron":
			if n := cronNext(t.Cron); n != nil {
				next = n
			}
		case "cdc", "watch":
			watch = true
		}
	}

	s.sched.register(key, spec.Owner, next, syncFn)
	if onStart {
		s.sched.trigger(key)
	}
	if watch {
		s.startWatch(spec, sink)
	}
}

// JobCount returns the number of registered sync jobs + active watch
// streams (observability + tests).
func (s *Service) JobCount() int {
	s.sched.mu.Lock()
	n := len(s.sched.jobs)
	s.sched.mu.Unlock()
	s.mu.Lock()
	n += len(s.watches)
	s.mu.Unlock()
	return n
}

func (s *Service) Deregister(spec SourceSpec) {
	key := stateKey(spec)
	s.sched.deregister(key)
	s.mu.Lock()
	if cancel := s.watches[key]; cancel != nil {
		cancel()
		delete(s.watches, key)
	}
	s.mu.Unlock()
	s.sfMu.Lock()
	delete(s.sfLocks, key)
	s.sfMu.Unlock()
}

// Remove permanently tears a source down : it deregisters it AND releases the
// connector's durable server-side resources (CDC slot/publication) so a
// deleted app stops accumulating WAL on its source database. Use this for an
// explicit "delete source", NOT for idle engine eviction (that uses Deregister
// and keeps the slot so the source resumes durably when it next runs).
func (s *Service) Remove(ctx context.Context, spec SourceSpec) error {
	s.Deregister(spec)
	conn, ok := connectorFor(spec.Type)
	if !ok {
		return nil
	}
	if c, ok := conn.(Cleanupable); ok {
		return c.Cleanup(ctx, spec)
	}
	return nil
}

func (s *Service) startWatch(spec SourceSpec, sink Sink) {
	conn, ok := connectorFor(spec.Type)
	if !ok || !conn.Capabilities().Watch {
		return
	}
	key := stateKey(spec)
	s.mu.Lock()
	if _, exists := s.watches[key]; exists {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.watches[key] = cancel
	s.mu.Unlock()
	metered := meteredSink{Sink: sink, m: s.metrics}
	go s.superviseWatch(ctx, conn, spec, metered, s.cursorFor(spec))
}

// superviseWatch keeps a Watch stream alive : it restarts the connector with
// exponential backoff whenever it returns an error (a dropped Kafka broker, a
// reset replication connection), until the source is deregistered. Durable
// cursors (Kafka offset, CDC LSN) make each restart resume where it left off.
func (s *Service) superviseWatch(ctx context.Context, conn Connector, spec SourceSpec, sink Sink, cur Cursor) {
	defer func() { _ = recover() }()
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		err := conn.Watch(ctx, spec, sink, cur)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.metrics.inc(&s.metrics.WatchErrors)
		}
		s.metrics.inc(&s.metrics.WatchRestarts)
		// A stream that ran healthy for a while before dropping resets the
		// backoff, so recovery resumes at full speed instead of carrying a
		// stale escalated delay (real-time CDC/Kafka latency after a flap).
		if time.Since(start) >= s.watchReset {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// meteredSink counts Watch-stream documents (the Sync path counts its own).
type meteredSink struct {
	Sink
	m *Metrics
}

func (w meteredSink) Upsert(ctx context.Context, kb string, docs []Document) error {
	err := w.Sink.Upsert(ctx, kb, docs)
	if err != nil {
		w.m.inc(&w.m.WatchErrors)
		return err
	}
	w.m.add(&w.m.DocsUpserted, int64(len(docs)))
	w.m.add(&w.m.WatchEvents, int64(len(docs)))
	return nil
}

func (w meteredSink) Delete(ctx context.Context, kb, id string) error {
	err := w.Sink.Delete(ctx, kb, id)
	if err == nil {
		w.m.inc(&w.m.DocsDeleted)
		w.m.inc(&w.m.WatchEvents)
	}
	return err
}

func (s *Service) loadHashes(cur Cursor, key string) map[string]string {
	b, _ := cur.Load(key)
	if len(b) == 0 {
		return map[string]string{}
	}
	m := map[string]string{}
	_ = json.Unmarshal(b, &m)
	return m
}

func (s *Service) saveHashes(cur Cursor, key string, m map[string]string) {
	b, _ := json.Marshal(m)
	_ = cur.Save(key, b)
}

func stateKey(spec SourceSpec) string {
	return spec.Owner + "\x00" + spec.KB + "\x00" + spec.Type + "\x00" + spec.Name
}

func docHash(d Document) string {
	keys := make([]string, 0, len(d.Meta))
	for k := range d.Meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(d.Text)
	for _, k := range keys {
		b.WriteByte('\x00')
		b.WriteString(k)
		b.WriteByte('=')
		fmt.Fprint(&b, d.Meta[k])
	}
	h := sha1.Sum([]byte(b.String()))
	return hex.EncodeToString(h[:])
}
