# Digitorn Indexation Service — DESIGN

Status legend: ✅ exists/reused · 🔶 partial · ⬜ planned.

A **generic, domain-agnostic, ultra-scalable indexation service**. Consumers
(the RAG module, the codebase code-intel layer, anything future) declare in
**YAML** *what* to index (a source) and *when* (triggers); the service fetches,
detects changes, syncs incrementally, and streams documents to the consumer's
sink. It **never runs on the daemon loop**, and it is **100% backward-compatible**
with the existing (and old Python daemon) RAG config — new power is additive.

---

## 0. Goals / non-goals

**Goals**
- One service indexes **everything**, organised **by domain** (web, files,
  database, codebase, streams/Kafka, SaaS…), each backed by the best pure-Go lib.
- **Fully configurable end-to-end from YAML** — no per-source code in the RAG.
- Every **trigger** kind: `on_start`, `interval`, **`cron`**, **`cdc`** (DB WAL/
  binlog), **`watch`** (Kafka / file-watch), `manual`, `webhook`.
- **Scale**: 100k apps, hundreds of sessions each, Amazon-scale sites — bounded
  resources, incremental, durable cursors, error isolation.
- **NEVER block / slow the daemon** (the #1 invariant).
- **Backward compatible**: current RAG config + old Python daemon config keep
  working unchanged.

**Non-goals (v1)**
- Full-site asset mirroring (images/CSS) — we index **text** for retrieval, not a
  byte-for-byte HTTrack mirror.
- Cross-machine distributed crawl frontier (single-node bounded for now; sharding
  is a documented future extension).

---

## 1. The never-block invariant (non-negotiable)

- For the **RAG**: the service runs inside the **rag worker** (a separate OS
  process). The daemon is never on the indexation path.
- All heavy work (crawl / extract / embed / CDC stream) runs on **async, bounded**
  goroutines — never on a request/response path.
- `watch`/`cdc`/`kafka` are **long-lived bounded** goroutines in the worker, with
  backpressure, not on the loop.
- For **in-proc consumers** (code-intel in the daemon): the service exposes only
  async, bounded entry points (goroutine pool + LRU + TTL), exactly like the
  current sindex/cgraph managers — the hot loop never calls a connector
  synchronously.

---

## 2. Core abstractions

```go
package indexer

// Document is one indexed unit from any source.
type Document struct {
    ID   string         // stable per-source id: URL, file path, row pk, symbol key, kafka key
    Text string
    Meta map[string]any // filterable metadata (url, title, lang, mtime, table, …)
}

// SourceSpec is the fully-decoded, validated config for one source.
type SourceSpec struct {
    Name string
    Type string                 // "web" | "file" | "database" | "kafka" | "codebase" | …
    KB   string                 // target knowledge base
    Opts map[string]any         // connector-specific options (typed per connector)
    Triggers []Trigger
}

// Connector indexes one domain. Walk = full/incremental scan (pull);
// Watch = continuous change stream (push). A connector implements at least
// one. Both MUST be cancellable + bounded; neither touches the daemon loop.
type Connector interface {
    Type() string
    Walk(ctx context.Context, spec SourceSpec, emit func(Document) error) error
    Watch(ctx context.Context, spec SourceSpec, sink Sink, cursor Cursor) error // optional
    Capabilities() Caps // {Walk, Watch, NeedsCursor}
}

// Sink is the consumer. RAG's sink = chunk+embed+store; code-intel = graph.
type Sink interface {
    Upsert(ctx context.Context, kb string, docs []Document) error
    Delete(ctx context.Context, kb, id string) error
}

// Cursor is durable per-source sync state: content hashes (Walk) OR a native
// position (Kafka offset, Postgres LSN, MySQL binlog pos). Survives restart.
type Cursor interface {
    Load(key string) ([]byte, error)
    Save(key string, state []byte) error
}

// registry
func Register(c Connector)
```

The **Service** owns the scheduler, the cursor store, and the run loop. A
consumer builds it once with its Sink + Cursor + deps (embedder lives in the
sink), declares sources, and the service drives everything.

---

## 3. Connectors (one per domain, best pure-Go lib)

| Domain | Lib | Walk | Watch | Notes |
|---|---|---|---|---|
| **web** | **gocolly/colly** | ✅ | — | async, rate-limit/host, robots.txt, depth, include/exclude regex, **sitemap.xml** seeding for huge sites, redirects, resume frontier. (chromedp/Geziyor opt-in for JS) |
| **file / document** | **tsawler/tabula** | ✅ | (fsnotify) | pure-Go PDF/DOCX/XLSX/PPTX/HTML/EPUB/ODT + layout + RAG chunking |
| **database** | jackc/pgx · go-mysql | ✅ | **CDC** | Walk = SQL query (have it ✅). Watch = **pglogrepl** (Postgres WAL) / **go-mysql** (binlog) → real-time |
| **kafka / stream** | segmentio/kafka-go | — | ✅ | consume a topic; each message → doc; durable **offset** cursor |
| **codebase** | tree-sitter (✅) + fsnotify + git | ✅ | ✅ | AST chunks + dep graph (reuse code-intel); Watch = git-diff/fsnotify |
| **filesystem dir** | os + tabula | ✅ | fsnotify | reuse current file source ✅ |

Each connector decodes its own `Opts` (typed), validates, and is the ONLY place
that domain knows how to be fetched. Adding a domain = one new connector file.

---

## 4. Triggers (the "when")

| Trigger | Meaning | Engine |
|---|---|---|
| `on_start` | sync once when the engine warms | immediate dispatch |
| `interval` | every N (`every: 6h`) | timer in the bounded scheduler |
| **`cron`** | cron expression (`expr: "0 3 * * *"`) | **robfig/cron** parser, next-fire in scheduler |
| **`cdc`** | DB change stream (Postgres WAL / MySQL binlog) | long-lived Watch goroutine + durable LSN/pos |
| **`watch`** | Kafka topic / file-watch | long-lived Watch goroutine + durable offset |
| `manual` | a tool/API call triggers a sync now | on-demand dispatch |
| `webhook` | external POST → sync | daemon route → worker dispatch (future) |

Triggers are **composable** per source (e.g. `cdc` for real-time + `interval` as
a safety re-sync). Pull triggers (`on_start`/`interval`/`cron`/`manual`) run a
`Walk` diff; push triggers (`cdc`/`watch`) run a `Watch` stream.

---

## 5. Config schema + BACKWARD COMPATIBILITY (critical)

The service is configured under `rag.config` (or any consumer's config). **Three
config generations must all parse and behave identically where they overlap:**

### (a) OLD Python daemon form — must keep working
Flat keys, no `config:` wrapper, Python source schema:
```yaml
tools:
  modules:
    rag:
      backend: { type: qdrant, url: ... }      # flat (no config:)
      embedding_model: bge-m3
      sources:
        - { type: file, path: ./docs }
      auto_index: { on_start: true, schedule: 6h }
```
→ Handled by the existing/added flat-capture `UnmarshalYAML` on `ModuleBlock`
(🔶 pending retrocompat item) + the mappings below. **No app rewrite required.**

### (b) CURRENT form — must keep working (what's built today)
```yaml
rag:
  config:
    backend: { type: qdrant }
    sources:
      - { type: file, path: ./docs, knowledge_base: docs }
      - { type: database, dsn: ..., query: ..., id_column: id }
    auto_index: { on_start: true, schedule: 6h }
```

### (c) NEW power — additive, opt-in
```yaml
rag:
  config:
    sources:
      - name: qdrant-docs
        type: web
        url: https://qdrant.tech/documentation
        crawl: { max_pages: 500, max_depth: 5, same_domain: true,
                 include: ["/documentation/"], exclude: ["\\.(png|svg)$"],
                 rate_limit: 200ms, parallelism: 4, respect_robots: true, sitemap: true }
        triggers:
          - { type: cron, expr: "0 3 * * *" }
          - { type: on_start }
        knowledge_base: docs
```

### Compatibility rules (so old configs map to the new engine)
- A source with **no `triggers:`** falls back to the global `auto_index`:
  `auto_index.on_start:true` → `{type:on_start}` ; `auto_index.schedule:"6h"`
  → `{type:interval, every:6h}` (or `{type:cron}` if the value is a cron expr).
- Existing `type: file` / `type: database` source fields are unchanged; new
  fields (`crawl`, `triggers`, `name`, kafka/cdc opts) are additive and optional.
- `SourceConfig` gains the new fields but every old field keeps its meaning.
  Unknown keys are tolerated (already the rule).
- The current in-engine `auto_index` ticker behaviour is **replaced internally**
  by the shared scheduler, but the **observable behaviour is identical** for
  existing configs (on_start syncs, schedule re-syncs).

**Net:** any app written for the old daemon or for today's RAG keeps working
byte-for-byte; the new connectors/triggers only activate when their keys appear.

---

## 6. Sync engine — incremental + durable

- **Pull (Walk)**: connector emits the current doc set; the service diffs each
  doc's content hash against the stored `Cursor` → `sink.Upsert(changed)` +
  `sink.Delete(removed)`. (This generalises today's `applySync`. ✅)
- **Push (Watch)**: connector streams native change events (Kafka msg, WAL row,
  fsnotify) → `sink.Upsert/Delete` directly, advancing a durable position
  (offset / LSN / binlog pos) so a restart resumes exactly where it left off.
- **Cursor store**: pluggable. v1 = the daemon's existing KV/DB (per
  `app:source` key). Survives worker restarts.
- **Idempotent**: doc IDs are stable; re-running a sync never duplicates.

---

## 7. Scale model (100k apps · huge sites · 1M sessions)

- **One bounded worker pool** for ALL sync jobs across ALL apps (global
  concurrency cap, e.g. min(16, cores)). 100k apps ≠ 100k goroutines.
- **single-flight** per source; a still-running sync is never restarted.
- **Connector/engine LRU + TTL**: only hot apps hold connections; cold apps'
  jobs deregister → resources freed. Re-warm on next use.
- **Crawl**: bounded pages/depth/bytes/time + per-host rate-limit & concurrency;
  **sitemap-first** for huge catalogs (Amazon-scale) instead of blind link
  walking; resumable frontier cursor.
- **Incremental everywhere**: only changed docs are re-embedded (hash / ETag /
  Last-Modified / CDC) → embedding cost scales with *change*, not corpus size.
- **Backpressure**: Watch streams block on a bounded channel rather than
  unbounded buffering; slow sinks throttle the source.
- **Error isolation + retry/backoff**: one failing source never affects another;
  failures are logged + retried, the rest keep flowing.
- **Query path (1M sessions)** is untouched by indexation — reads hit the vector
  backend + semantic cache; writes (sync) are isolated in the worker pool.
- **Future**: shard the crawl frontier / partition apps across multiple rag
  worker instances via a claim table (documented, not v1).

---

## 8. Integration

- **RAG** provides a `Sink` = `IngestWithMeta` (chunk+embed+store) + a delete via
  `DeleteBySource`. Its `engineFor` registers the app's sources with the service.
  `SyncSource`/`SyncAll` become thin calls into the service. ACL/cache/migrate
  are unchanged (they live in the RAG engine, downstream of the sink).
- **Code-intel** can provide a different `Sink` (symbol graph) and use the same
  `codebase` connector + `watch` trigger — one service, two consumers.

---

## 9. Build plan (each slice proven live before the next)

1. **Core** `internal/indexer` (Connector/Trigger/Sink/Service/Cursor + bounded
   single-flight scheduler + cron via robfig/cron) **+ Web connector (Colly)** +
   triggers on_start/interval/cron. Wire RAG sources through it (back-compat
   mappings). **Prove on Qdrant docs, live.**
2. **Document connector (Tabula)** → replaces hand-rolled loaders.
3. **Database + CDC** (pglogrepl Walk+Watch).
4. **Kafka** connector (`watch`).
5. **Codebase** connector (tree-sitter + fsnotify) → also powers code-intel.

---

## 10. Decisions (validated 2026-06-11)

- **Cursor store** = reuse the **daemon KV/DB via the gateway** (extend the
  gateway with a small KV service, like the embedder). Durable + centralised +
  survives worker restarts. → `Cursor` interface with an in-memory impl for the
  first proof, gateway-KV impl wired within Phase 1.
- **Web crawl** = **sitemap-first, fallback to link-crawl**. If a sitemap.xml
  exists it seeds the frontier (faster + complete on Amazon-scale sites); else
  BFS from the seed.
- **Manual re-index** = **pure admin/config** (control plane), NOT an agent tool.
  Re-index happens via triggers (cron/interval/cdc/watch) or an admin call.

Still open (later phases): webhook trigger (needs daemon route→worker hop);
JS-rendering via chromedp (opt-in per source); cross-worker frontier sharding.
