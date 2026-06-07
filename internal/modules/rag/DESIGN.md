# RAG Production Design (2026)

Reference design for the Digitorn RAG module: a production-grade,
multi-tenant retrieval service any company can integrate by pointing it
at **its own** data sources and vector backend. Aligned with 2026
standards (permission-aware retrieval at the vector layer, incremental
sync with lineage, hybrid + cross-encoder rerank, contextual retrieval,
CRAG/adaptive, RAGAS eval).

Status legend: ✅ done+proven · 🔶 partial · ⬜ planned.

## 1. Principles

- **The store belongs to the app.** We connect to the app's vector
  server (Qdrant/pgvector/…) anywhere; we never host it. Backend is a
  YAML config (retrocompat with the old Python keys). ✅
- **Worker-isolated.** All RAG compute runs in a worker; the daemon loop
  is never blocked. Embeddings/rerank/LLM reached via the service
  gateway. ✅
- **Retrieval is the bottleneck** (≈73% of RAG failures). Invest in
  sources, chunking, hybrid+rerank, governance — not the LLM.
- **LLM/model-agnostic.** Embedding/rerank/LLM are pluggable.
- **Security is first-class.** Permission filtering happens IN the vector
  query (filter-first, pre-rerank), never as app-layer post-filtering.

## 2. Three planes

```
CONTROL PLANE (admin/config)          INGESTION+SYNC PLANE (async)         RETRIEVAL PLANE (online, ms)
 register KB                           Source connectors                    query understanding (rewrite/route)
 attach Sources (+creds)               → load (PDF/DOCX/DB rows/…)          → hybrid (dense+BM25, ACL filter-first)
 schedule/trigger sync                 → clean → chunk (recursive)          → RRF fuse
 delete KB/Source                      → enrich (contextual, metadata)      → cross-encoder rerank
 observe (status, metrics, eval)       → embed (batch) → upsert             → compress/reorder
                                        incremental (CDC/watermark/watch)    → grounded generate + citations
                                        lineage doc→chunks, tombstones       → CRAG grade → fallback / adaptive skip
```

## 3. Data model + lineage

```go
// KnowledgeBase: one collection on the app's backend, scoped per (app[,user]).
type KB struct { ID, AppID, Tenant string; EmbedModel string; Dim int; Version int }

// Source: a declared, continuously-synced origin attached to a KB.
type Source struct {
    ID, KBID string
    Type     string            // "file" | "database" | "web"
    Config   map[string]any    // path/dsn/url, connection_id, tables, extensions…
    Sync     SyncConfig        // strategy + interval
    State    SyncState         // watermark/cursor, last run, counts (persisted)
}

// Document: one logical unit from a source (a file, a DB row, a page).
type Document struct {
    ID       string            // stable per (source, natural-key) → updates replace
    KBID, SourceID string
    Hash     string            // change detection
    Version  int
    ACL      []string          // principals allowed to retrieve (groups/users)
    Tenant   string
    Meta     map[string]any
}

// Chunk: indexed unit; references its parent Document for lineage + citation.
type Chunk struct {
    ID       string            // deterministic (doc, index)
    DocID, KBID string
    Vector   []float32
    Text     string
    Index    int               // position in doc (citation)
    ACL      []string          // copied from doc → filter-first at query
    Tenant   string
    Meta     map[string]any    // source, title, page, version…
}
```

Rules: re-ingesting a Document **replaces** its chunks (no dup). Deleting
a Document/Source **tombstones** then removes its chunks. Changing
embed model/chunking bumps `KB.Version` → re-index.

## 4. Core interfaces (Go)

```go
// VectorBackend — the app's store. Filter enables ACL/metadata filter-first.
type VectorBackend interface {
    EnsureKB(ctx, kb string, dim int) error
    DeleteKB(ctx, kb string) error
    ListKBs(ctx) ([]string, error)
    CountKB(ctx, kb string) (int, error)
    Upsert(ctx, kb string, docs []Chunk) error
    DeleteByDoc(ctx, kb, docID string) error            // ⬜ lineage delete
    Search(ctx, kb string, vec []float32, topK int, f Filter) ([]SearchHit, error) // 🔶 + Filter
    Scan(ctx, kb string, f Filter) ([]Chunk, error)
    Discover(ctx) ([]string, error)                     // ⬜ pre-built index
    Close() error
}

// Filter — ACL + metadata, pushed into the backend query (NOT post-filtered).
type Filter struct {
    AnyACL []string          // principal set of the caller (OR match against chunk.ACL)
    Meta   map[string]any    // equality/range metadata filters
}

// Source connector — pluggable origin with incremental change detection.
type Connector interface {
    Type() string
    // Changes returns docs added/updated since state, deletions, and the new state.
    Changes(ctx, src Source) (upserts []LoadedDoc, deletes []string, next SyncState, err error)
}
type LoadedDoc struct { Doc Document; Text string }  // text → chunk→embed in the engine

// SyncEngine — orchestrates connectors → chunk → enrich → embed → upsert, incrementally.
type SyncEngine interface {
    SyncNow(ctx, kb string, sourceID string) (SyncReport, error)
    Schedule(kb, sourceID string, every time.Duration)
    Stop()
}
```

Embedder/Reranker/LLM are injected via ctx from the gateway
(`module.EmbedderFrom`, `module.RerankerFrom`, `module.CallerFrom`). ✅ (LLM ⬜)

## 5. Ingestion + sync

- **Incremental change detection** per source:
  - `file`: walk + mtime/checksum; deletes by set-difference.
  - `database`: `updated_at` watermark | changelog table | LISTEN/NOTIFY (CDC). ⬜
  - `web`: ETag/Last-Modified.
- **Pipeline**: Connector.Changes → for each LoadedDoc: chunk (recursive)
  → **contextual enrich** (LLM prepends doc context per chunk, +5–15%; cached) ⬜
  → embed (batch) → Upsert(chunks). Deletes → DeleteByDoc.
- **Idempotent + versioned**: doc Hash skips unchanged; KB.Version guards re-index.
- **State persisted** (GORM) so sync resumes after restart.

## 6. Retrieval pipeline

1. **Query understanding** ⬜: optional rewrite / multi_query / route (which KB).
2. **Hybrid** ✅: dense (embed query) + BM25, each top-N (50–200).
   - **ACL filter-first** ⬜: pass caller principals as `Filter.AnyACL` so the
     backend prunes by permission BEFORE similarity.
3. **RRF fuse** ✅ (k=60, weighted by bm25_weight/semantic_weight).
4. **Rerank** ✅: cross-encoder over fused top rerank_top_n → final_top_k.
   (ColBERT late-interaction = optional upgrade ⬜.)
5. **Assemble** ⬜: compress/prune, reorder (lost-in-the-middle), dedup.
6. **Generate** ⬜: grounded answer + citations; **CRAG** grade retrieval
   confidence → re-query/fallback; **adaptive** skip retrieval when not needed.

## 7. Security / multi-tenancy

- KB scoped per (app[,user]) via YAML. ✅
- Chunks carry `Tenant` + `ACL`; **query filters by caller principals at the
  vector layer** (filter-first). ⬜ — the production differentiator.
- Per-source credentials via the secrets store; never in the index.
- Retrieved content is untrusted (prompt-injection): isolate in generation.

## 8. Eval / ops ⬜

- **RAGAS-style** harness: faithfulness, answer-relevancy, context
  precision/recall (targets ~0.75/0.8/0.7/0.8). Golden set per app.
- Observability: retrieval latency, recall@k, rerank scores, token cost.
- **Semantic cache** of query→results (cosine on query embedding; invalidated on re-index).

## 9. Tool surface — agent-exposed vs INTERNAL

The **control plane is internal / config-driven, NOT exposed to the LLM
agent.** Sources are declared in the app YAML
(`tools.modules.rag.config.sources: [...]` + `auto_index`, retrocompat
with the old Python) and the internal **SyncEngine** indexes/syncs them
automatically (on_start + schedule + CDC). The agent never manages
sources/sync.

- **Agent-exposed (LLM tools)**: `query` ✅ · `ingest` (ad-hoc text) ✅ ·
  `ingest_file` (ad-hoc) ⬜ · `multi_query` ⬜ · `sql_query` ⬜.
- **Internal / config-driven (NOT agent tools)**: `sources` + SyncEngine
  (file/dir/database/codebase connectors, incremental, CDC) ⬜ ·
  `migrate_embeddings` ⬜ · `clear_cache` ⬜ · KB lifecycle
  (create/list/stats/delete — admin) 🔶 currently agent tools, may move
  to the control plane.
- **Control-plane surface** (admin/config, not LLM): attach/sync/list
  sources is done via YAML + lifecycle, optionally a REST admin API
  later — never a meta-tool the agent calls.

## 10. Backends (pluggable, 2026)

✅ Qdrant (native gRPC). ⬜ pgvector (if app uses Postgres), Chroma,
Pinecone, Elasticsearch/Weaviate (native hybrid), LanceDB. All behind
VectorBackend; HNSW + int8/binary quantization where supported.
`Discover` connects to a pre-built index (query-only).

## 11. Models (2026)

- Embeddings: minilm-l12 (384) ✅, bge-m3 (1024) ✅; add Qwen3-Embedding,
  Nemotron, mxbai ⬜; **Matryoshka** dim-truncation knob ⬜.
- Rerank: bge-reranker-base (cross-encoder) ✅.
- All ONNX on our runtime (Windows-safe); custom models via registry.

## 12. Libraries — adopt vs build (researched, 2026)

No single Go all-in-one (powerful ones are Python). Assemble best-of-breed Go + our core:
- **CDC**: `pglogrepl` (Postgres WAL, pure-Go on pgx) + `go-mysql` (binlog). Pure-Go, no external infra (vs Debezium). ⬜
- **Keyword index**: **bleve** (pure-Go, native BM25, **persistent**, metadata filters, +vectors) → replaces hand-rolled in-mem BM25 (survives restart, ACL/metadata filters free). ⬜
- **Code chunking**: `smacker/go-tree-sitter` (CGO, Windows-OK like onnx) + AST/cAST structural chunking for the codebase connector. ⬜
- **Doc extraction**: pure-Go patchwork first (excelize XLSX, a PDF lib, html-to-markdown, plain text/md/code/csv/json) for Windows safety; `extractous-go` (Rust/native, 60+ formats+OCR) as optional power mode. ⬜
- **Agentic orchestration**: evaluate `cloudwego/eino` (ByteDance, components + graph + ADK, 37 deps) for P5 only; keep our worker/gateway/backend core. ⬜
- **Vector store**: app's own (Qdrant ✅ / pgvector / Weaviate…) — ANN + quantization done by the store.
- **Eval**: no Go RAGAS — implement faithfulness/context-recall metrics ourselves. ⬜

## Non-functional guarantees

- **Never block the daemon loop**: all RAG compute in the worker; SyncEngine is async (goroutine pool + queue + backpressure + concurrency caps); ingestion never touches the loop.
- **Ultra-fast**: batched embeddings, bounded candidate sets (top-N→rerank), semantic cache, ANN+quantization in the backend, persistent bleve (no rebuild), int8 reranker.
- **Connect to anything**: Connector set = database (CDC), files/dir, **codebase (git + tree-sitter AST)**, web/API.
- **Antagonist/self-critique**: CRAG (grade retrieval confidence → re-query/fallback) + Self-RAG (critique own answer) + multi-judge verification.

## 12b. Code Intelligence Engine (codebase — "total comprehension, beat Cursor")

A dedicated code mode, not generic chunking. Goal: the agent has total
codebase comprehension via navigation, not just chunk retrieval.

**Assets we already have**: `internal/csearch` (trigram index = fast
exact/regex identifier search — reuse for the lexical side) · `lsp`
module (diagnostics today; extend for def/refs later) · embeddings worker
· the RAG engine + SyncEngine.

**Pipeline**:
1. **Connector** (codebase): git-aware walk (respect .gitignore), incremental via SyncEngine/git-diff. ⬜
2. **AST chunking** (tree-sitter / `smacker/go-tree-sitter`, CGO): chunk by function/class/method (cAST), never line-splits a function. Each chunk's metadata = path, language, symbol name + kind + signature, line range, parent (file/class). ⬜
3. **Code graph** (tree-sitter symbols + imports/calls; optionally LSP go-to-def/find-refs for precision): definitions, references, callers, imports → traversable graph for "who calls / where defined / what depends". This is the differentiator. ⬜
4. **Multi-index retrieval**, fused (RRF) + code rerank:
   - semantic: **code embeddings** over AST chunks (model decision below),
   - exact/regex: **csearch trigram** (identifiers, literals),
   - symbol: name→definition lookup,
   - graph: traverse references/callers/imports. ⬜
5. **Agentic navigation tools** (these ARE agent tools — the exploration Cursor/Claude-Code do): `code_search` (hybrid), `find_symbol` (go-to-def), `find_references` (callers/usages), `outline` (file symbols), `expand` (chunk→full func/file), `grep` (regex via csearch). Total comprehension = the agent traverses the graph. ⬜
6. **Incremental**: re-index only git-changed files. ⬜

**Code embedding model — DECISION NEEDED**: code retrieval needs a
code-tuned embedder (general minilm underperforms). Options:
(a) **self-host ONNX** (jina-embeddings-v2-base-code / CodeRankEmbed /
nomic-embed-code) → requires adding a **WordPiece tokenizer** to the
worker (these are BERT-based). Windows-safe, self-contained.
(b) **API voyage-code-3** (MTEB Code 84, best; +14-17% vs OpenAI; low-dim
quantized) → best quality but external API + cost, breaks self-contained.

## 13. Phase plan (each proven in real)

- **P0–P2 ✅** multi-model embeddings · service gateway · RAG module +
  Qdrant + hybrid BM25+RRF + cross-encoder rerank + citations + retrocompat.
- **P3 ⬜ Ingestion of record**: Source/Connector + SyncEngine
  (incremental, lineage, tombstones) + file/dir loaders (PDF/DOCX/MD/code/
  CSV/HTML) + ingest_file/dir + attach/sync/list_sources.
- **P4 ⬜ Governance + backends**: ACL/metadata **filter-first** (data-model
  + Search(Filter) + DeleteByDoc) + pgvector/Chroma/Pinecone/ES + Discover
  + semantic cache + migrate_embeddings.
- **P5 ⬜ Intelligence**: wire LLM into worker → contextual retrieval +
  multi_query + CRAG/adaptive (+ agentic loop) + database source (CDC) +
  text2sql/sql_query.
- **P6 ⬜ Quality**: RAGAS eval harness + observability + list_models.
