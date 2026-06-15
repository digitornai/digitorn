package indexer

import (
	"sync/atomic"
	"time"
)

// Metrics holds the indexation service's runtime counters (lock-free atomics)
// so a scrape never contends with the hot path. Snapshot exposes them for
// /metrics, expvar, or a health endpoint.
type Metrics struct {
	SyncsStarted  int64
	SyncsOK       int64
	SyncsFailed   int64
	DocsUpserted  int64
	DocsDeleted   int64
	DeadLettered  int64
	WatchEvents   int64
	WatchErrors   int64
	WatchRestarts int64
	LeaseSkipped  int64
	InFlight      int64

	lastSyncUnix int64
}

func newMetrics() *Metrics { return &Metrics{} }

func (m *Metrics) inc(p *int64) { atomic.AddInt64(p, 1) }

func (m *Metrics) add(p *int64, n int64) {
	if n != 0 {
		atomic.AddInt64(p, n)
	}
}

func (m *Metrics) gaugeAdd(p *int64, d int64) { atomic.AddInt64(p, d) }

func (m *Metrics) stampSync() { atomic.StoreInt64(&m.lastSyncUnix, time.Now().Unix()) }

// Stats is an immutable snapshot of the counters at one instant.
type Stats struct {
	SyncsStarted  int64 `json:"syncs_started"`
	SyncsOK       int64 `json:"syncs_ok"`
	SyncsFailed   int64 `json:"syncs_failed"`
	DocsUpserted  int64 `json:"docs_upserted"`
	DocsDeleted   int64 `json:"docs_deleted"`
	DeadLettered  int64 `json:"dead_lettered"`
	WatchEvents   int64 `json:"watch_events"`
	WatchErrors   int64 `json:"watch_errors"`
	WatchRestarts int64 `json:"watch_restarts"`
	LeaseSkipped  int64 `json:"lease_skipped"`
	InFlight      int64 `json:"in_flight"`
	Jobs          int   `json:"jobs"`
	Watches       int   `json:"watches"`
	LastSyncUnix  int64 `json:"last_sync_unix"`
}

func (m *Metrics) snapshot() Stats {
	return Stats{
		SyncsStarted:  atomic.LoadInt64(&m.SyncsStarted),
		SyncsOK:       atomic.LoadInt64(&m.SyncsOK),
		SyncsFailed:   atomic.LoadInt64(&m.SyncsFailed),
		DocsUpserted:  atomic.LoadInt64(&m.DocsUpserted),
		DocsDeleted:   atomic.LoadInt64(&m.DocsDeleted),
		DeadLettered:  atomic.LoadInt64(&m.DeadLettered),
		WatchEvents:   atomic.LoadInt64(&m.WatchEvents),
		WatchErrors:   atomic.LoadInt64(&m.WatchErrors),
		WatchRestarts: atomic.LoadInt64(&m.WatchRestarts),
		LeaseSkipped:  atomic.LoadInt64(&m.LeaseSkipped),
		InFlight:      atomic.LoadInt64(&m.InFlight),
		LastSyncUnix:  atomic.LoadInt64(&m.lastSyncUnix),
	}
}
