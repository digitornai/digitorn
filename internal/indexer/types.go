// Package indexer is Digitorn's generic, domain-agnostic indexation service.
// Consumers (the RAG module, the codebase code-intel layer, …) declare in
// config WHAT to index (a source + connector) and WHEN (triggers); the
// service fetches, detects changes, syncs incrementally and streams the
// changed documents to the consumer's Sink. It runs entirely off the daemon
// loop (in the worker, async + bounded). See DESIGN.md.
package indexer

import (
	"context"
	"sync"
	"time"
)

// Document is one indexed unit produced by any connector.
type Document struct {
	ID   string         // stable per-source id: URL, file path, row pk, symbol key…
	Text string         // extracted text the consumer will chunk/embed/index
	Meta map[string]any // filterable metadata (url, title, lang, table, mtime…)
}

// Trigger declares when a source is (re)synced.
type Trigger struct {
	Type  string         `json:"type"`            // on_start | interval | cron | cdc | watch | manual
	Every time.Duration  `json:"-"`               // interval
	Cron  string         `json:"cron,omitempty"`  // cron expression
	Opts  map[string]any `json:"opts,omitempty"`  // trigger-specific (cdc slot, kafka group…)
}

// SourceSpec is the fully-resolved config for one source.
type SourceSpec struct {
	Name      string
	Type      string         // connector type: web | file | database | kafka | codebase…
	KB        string         // target knowledge base
	Owner     string         // tenant/app id — used for per-app fair scheduling
	CursorDSN string         // per-app sync-state DB (empty → service default cursor)
	Opts      map[string]any // connector-specific options
	Triggers  []Trigger
}

// Caps tells the service which modes a connector supports.
type Caps struct {
	Walk  bool // full/incremental pull scan
	Watch bool // continuous push change-stream
}

// Connector indexes one domain. Walk emits the current document set (the
// service diffs it). Watch streams changes continuously into the sink,
// advancing a durable cursor. Both must be cancellable + bounded and never
// touch the daemon loop. A connector implements at least one mode.
type Connector interface {
	Type() string
	Capabilities() Caps
	Walk(ctx context.Context, spec SourceSpec, emit func(Document) error) error
	Watch(ctx context.Context, spec SourceSpec, sink Sink, cur Cursor) error
}

// Sink is the consumer of indexed documents. RAG's sink chunks+embeds+stores;
// the code-intel sink builds the symbol graph.
type Sink interface {
	Upsert(ctx context.Context, kb string, docs []Document) error
	Delete(ctx context.Context, kb, id string) error
}

// Cleanupable is an optional connector capability : release a source's durable
// server-side resources on PERMANENT removal (e.g. drop a CDC replication slot
// + publication so the upstream WAL stops accumulating). Not called on idle
// eviction, which must preserve the slot for a durable resume.
type Cleanupable interface {
	Cleanup(ctx context.Context, spec SourceSpec) error
}

// Cursor is durable per-source sync state — content hashes (Walk) or a native
// position like a Kafka offset / Postgres LSN (Watch). Survives restart.
type Cursor interface {
	Load(key string) ([]byte, error)
	Save(key string, state []byte) error
}

// Locker grants a distributed per-source lease so that, across N worker
// replicas, only one runs a given source's Walk sync at a time — no double
// work and no race on the shared cursor. ok=false means another holder owns
// it (skip this tick). The default single-node locker always grants. A Cursor
// that also implements Locker is used as the service's lease backend.
type Locker interface {
	Acquire(ctx context.Context, key string) (release func(), ok bool)
}

var (
	regMu    sync.RWMutex
	registry = map[string]Connector{}
)

// Register makes a connector available by its Type(). Called from connector
// init() so importing the package wires the domain.
func Register(c Connector) {
	regMu.Lock()
	registry[c.Type()] = c
	regMu.Unlock()
}

func connectorFor(typ string) (Connector, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	c, ok := registry[typ]
	return c, ok
}
