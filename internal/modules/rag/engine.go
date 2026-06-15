package rag

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"

	pkgmodule "github.com/mbathepaul/digitorn/pkg/module"
)

// Engine is the per-call RAG pipeline : chunk → embed (via the daemon
// gateway embedder) → store/search in the app's VectorBackend → cite.
// It is built per invocation from the app's resolved Config so a shared
// worker instance serves every app correctly.
type Engine struct {
	cfg      Config
	backend  VectorBackend
	embed    pkgmodule.Embedder
	reranker pkgmodule.Reranker
	model    string
	cache    *semCache

	mu       sync.Mutex
	idx      map[string]*kbIndex          // kb -> keyword index (BM25 + doc metadata)
	srcState map[string]map[string]string // sourceKey -> relpath -> stat-sig (incremental sync)
}

// kbIndex is the in-memory keyword side of a knowledge base : a BM25
// index plus the chunk metadata needed to resolve a hit back to a
// citation. Rebuilt from the vector store on cold start.
type kbIndex struct {
	mu   sync.RWMutex
	bm25 *BM25
	docs map[string]Document
}

func (ix *kbIndex) add(d Document) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.bm25.Add(d.ID, d.Text)
	ix.docs[d.ID] = Document{ID: d.ID, Text: d.Text, Source: d.Source, Chunk: d.Chunk, Meta: d.Meta}
}

func (ix *kbIndex) length() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.bm25.Len()
}

func (ix *kbIndex) search(query string, topN int) []bm25Hit {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.bm25.Search(query, topN)
}

func (ix *kbIndex) doc(id string) Document {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.docs[id]
}

// NewEngine wires an engine from a parsed Config, a backend connector,
// the embedder and (optionally) the reranker bridged in from the daemon.
func NewEngine(cfg Config, backend VectorBackend, embed pkgmodule.Embedder, reranker pkgmodule.Reranker) *Engine {
	e := &Engine{
		cfg: cfg, backend: backend, embed: embed, reranker: reranker, model: cfg.EmbeddingModel.ID,
		idx:      map[string]*kbIndex{},
		srcState: map[string]map[string]string{},
	}
	if cfg.Cache.Enabled {
		e.cache = newSemCache(cfg.Cache)
	}
	return e
}

// Close releases the engine's backend connection. Called when the engine is
// evicted from the module's LRU.
func (e *Engine) Close() error {
	if e.backend != nil {
		return e.backend.Close()
	}
	return nil
}

// invalidate marks a knowledge base's cached query results stale (called
// after any write to that KB). No-op when the semantic cache is off.
func (e *Engine) invalidate(kb string) {
	if e.cache != nil {
		e.cache.bump(kb)
	}
}

// Ingest chunks text, embeds the chunks as documents, ensures the KB
// exists at the right dimension, and upserts. Returns the chunk count.
func (e *Engine) Ingest(ctx context.Context, kb, text, source string) (int, error) {
	return e.IngestWithMeta(ctx, kb, text, source, nil)
}

// IngestWithMeta is Ingest with author-supplied metadata attached to every
// chunk. The ACL scope field is always overwritten from the caller, so an
// author can never stamp a document with someone else's owner.
func (e *Engine) IngestWithMeta(ctx context.Context, kb, text, source string, meta map[string]any) (int, error) {
	if e.embed == nil {
		return 0, fmt.Errorf("rag: embeddings unavailable (no gateway)")
	}
	chunks := Chunkize(text, e.cfg.Chunking.Strategy, e.cfg.Chunking.Size, e.cfg.Chunking.Overlap)
	if len(chunks) == 0 {
		return 0, nil
	}
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Text
	}
	vecs, dim, err := e.embed.EmbedModel(ctx, e.model, pkgmoduleRoleDocument, texts)
	if err != nil {
		return 0, fmt.Errorf("rag: embed: %w", err)
	}
	if len(vecs) != len(chunks) {
		return 0, fmt.Errorf("rag: embed returned %d vectors for %d chunks", len(vecs), len(chunks))
	}
	if err := e.backend.EnsureKB(ctx, kb, dim); err != nil {
		return 0, fmt.Errorf("rag: ensure kb: %w", err)
	}
	if source == "" {
		source = "inline"
	}
	docMeta := e.docMeta(ctx, meta)
	docs := make([]Document, len(chunks))
	for i, c := range chunks {
		docs[i] = Document{
			ID:     docID(kb, source, c.Index),
			Vector: vecs[i],
			Text:   c.Text,
			Source: source,
			Chunk:  c.Index,
			Meta:   docMeta,
		}
	}
	if err := e.backend.Upsert(ctx, kb, docs); err != nil {
		return 0, fmt.Errorf("rag: upsert: %w", err)
	}
	if e.usesBM25() {
		ix := e.indexFor(kb)
		for _, d := range docs {
			ix.add(d)
		}
	}
	e.invalidate(kb)
	return len(docs), nil
}

// SourceDoc is one document for batch ingestion.
type SourceDoc struct {
	ID, Text, Source string
	Meta             map[string]any
}

// IngestBatch chunks + embeds MANY documents with BATCHED embedding calls (one
// model forward per ~64 chunks instead of one per document) and writes them in
// a single backend upsert — the high-throughput path the indexer uses. At
// scale this is an order of magnitude faster than per-document ingestion.
func (e *Engine) IngestBatch(ctx context.Context, kb string, items []SourceDoc) (int, error) {
	if e.embed == nil {
		return 0, fmt.Errorf("rag: embeddings unavailable (no gateway)")
	}
	type ref struct{ item, chunk int }
	var texts []string
	var refs []ref
	itemChunks := make([][]Chunk, len(items))
	itemMeta := make([]map[string]any, len(items))
	itemSource := make([]string, len(items))
	for i, it := range items {
		cs := Chunkize(it.Text, e.cfg.Chunking.Strategy, e.cfg.Chunking.Size, e.cfg.Chunking.Overlap)
		itemChunks[i] = cs
		itemMeta[i] = e.docMeta(ctx, it.Meta)
		src := it.Source
		if src == "" {
			src = it.ID
		}
		if src == "" {
			src = "inline"
		}
		itemSource[i] = src
		for ci, c := range cs {
			texts = append(texts, c.Text)
			refs = append(refs, ref{i, ci})
		}
	}
	if len(texts) == 0 {
		return 0, nil
	}
	const batch = 256 // large enough to keep the embedding session pool saturated
	vecs := make([][]float32, len(texts))
	dim := 0
	for off := 0; off < len(texts); off += batch {
		end := min(off+batch, len(texts))
		bv, d, err := e.embed.EmbedModel(ctx, e.model, pkgmoduleRoleDocument, texts[off:end])
		if err != nil {
			return 0, fmt.Errorf("rag: embed batch: %w", err)
		}
		if len(bv) != end-off {
			return 0, fmt.Errorf("rag: embed returned %d vectors for %d texts", len(bv), end-off)
		}
		dim = d
		copy(vecs[off:end], bv)
	}
	if err := e.backend.EnsureKB(ctx, kb, dim); err != nil {
		return 0, fmt.Errorf("rag: ensure kb: %w", err)
	}
	out := make([]Document, len(refs))
	for k, r := range refs {
		c := itemChunks[r.item][r.chunk]
		out[k] = Document{
			ID:     docID(kb, itemSource[r.item], c.Index),
			Vector: vecs[k],
			Text:   c.Text,
			Source: itemSource[r.item],
			Chunk:  c.Index,
			Meta:   itemMeta[r.item],
		}
	}
	if err := e.backend.Upsert(ctx, kb, out); err != nil {
		return 0, fmt.Errorf("rag: upsert: %w", err)
	}
	if e.usesBM25() {
		ix := e.indexFor(kb)
		for _, d := range out {
			ix.add(d)
		}
	}
	e.invalidate(kb)
	return len(out), nil
}

// usesBM25 reports whether the keyword index is needed. Semantic-only retrieval
// skips the in-memory BM25 index entirely, so a large corpus does not pin its
// full text in daemon RAM.
func (e *Engine) usesBM25() bool {
	if strings.EqualFold(strings.TrimSpace(e.cfg.Pipeline.Retrieval), "semantic") {
		return false
	}
	if _, ok := e.backend.(KeywordSearcher); ok {
		return false // keyword search delegated server-side ; no in-RAM index
	}
	return true
}

func (e *Engine) indexFor(kb string) *kbIndex {
	e.mu.Lock()
	defer e.mu.Unlock()
	ix := e.idx[kb]
	if ix == nil {
		ix = &kbIndex{bm25: NewBM25(), docs: map[string]Document{}}
		e.idx[kb] = ix
	}
	return ix
}

// ensureIndex returns the keyword index for kb, rebuilding it from the
// vector store when empty (cold start / fresh worker).
func (e *Engine) ensureIndex(ctx context.Context, kb string) (*kbIndex, error) {
	ix := e.indexFor(kb)
	if ix.length() > 0 {
		return ix, nil
	}
	docs, err := e.backend.Scan(ctx, kb)
	if err != nil {
		return nil, fmt.Errorf("rag: scan: %w", err)
	}
	for _, d := range docs {
		ix.add(d)
	}
	return ix, nil
}

// Query retrieves the topK chunks for a question per the configured
// retrieval mode : semantic (vectors), bm25 (keyword), or hybrid (both,
// fused with Reciprocal Rank Fusion). topK falls back to final_top_k.
func (e *Engine) Query(ctx context.Context, kb, query string, topK int) ([]SearchHit, error) {
	if topK <= 0 {
		topK = e.cfg.Pipeline.FinalTopK
	}
	rerank := e.cfg.RerankEnabled && e.reranker != nil
	retrieveK := topK
	if rerank && e.cfg.Pipeline.RerankTopN > retrieveK {
		retrieveK = e.cfg.Pipeline.RerankTopN
	}

	acl := e.aclValue(ctx)
	filter := e.aclFilter(acl)
	mode := strings.ToLower(e.cfg.Pipeline.Retrieval)
	needSemantic := mode != "bm25"

	var qvec []float32
	if needSemantic {
		v, err := e.embedQuery(ctx, query)
		if err != nil {
			return nil, err
		}
		qvec = v
		if e.cache != nil {
			if hits, ok := e.cache.get(kb, acl, topK, qvec); ok {
				return hits, nil
			}
		}
	}

	var hits []SearchHit
	var err error
	switch mode {
	case "bm25":
		hits, err = e.bm25Search(ctx, kb, query, retrieveK, filter)
	case "semantic":
		hits, err = e.semanticSearchVec(ctx, kb, qvec, retrieveK, filter)
	default:
		hits, err = e.hybridSearchVec(ctx, kb, query, qvec, retrieveK, filter)
	}
	if err != nil {
		return nil, err
	}
	if rerank && len(hits) > 1 {
		hits = e.rerankHits(ctx, query, hits)
	}
	if len(hits) > topK {
		hits = hits[:topK]
	}
	if e.cache != nil && needSemantic {
		e.cache.put(kb, acl, topK, qvec, hits)
	}
	return hits, nil
}

// rerankHits re-scores hits with the cross-encoder and reorders them.
// A reranker failure degrades to the original order (graceful).
func (e *Engine) rerankHits(ctx context.Context, query string, hits []SearchHit) []SearchHit {
	docs := make([]string, len(hits))
	for i, h := range hits {
		docs[i] = h.Text
	}
	scores, err := e.reranker.Rerank(ctx, e.cfg.RerankModel, query, docs)
	if err != nil || len(scores) != len(hits) {
		return hits
	}
	for i := range hits {
		hits[i].Score = scores[i]
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	return hits
}

func (e *Engine) embedQuery(ctx context.Context, query string) ([]float32, error) {
	if e.embed == nil {
		return nil, fmt.Errorf("rag: embeddings unavailable (no gateway)")
	}
	vecs, _, err := e.embed.EmbedModel(ctx, e.model, pkgmoduleRoleQuery, []string{query})
	if err != nil {
		return nil, fmt.Errorf("rag: embed query: %w", err)
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("rag: embed query returned no vector")
	}
	return vecs[0], nil
}

func (e *Engine) semanticSearchVec(ctx context.Context, kb string, vec []float32, topK int, filter Filter) ([]SearchHit, error) {
	if len(vec) == 0 {
		return nil, fmt.Errorf("rag: empty query vector")
	}
	return e.backend.Search(ctx, kb, vec, topK, filter)
}

// keywordHits returns BM25/keyword hits for a query, delegated to the backend's
// native full-text search when it supports it (no in-RAM corpus), else served
// from the in-process BM25 index. Hits carry their Document + score and have
// already passed the metadata filter.
func (e *Engine) keywordHits(ctx context.Context, kb, query string, n int, filter Filter) ([]SearchHit, error) {
	if ks, ok := e.backend.(KeywordSearcher); ok {
		return ks.KeywordSearch(ctx, kb, query, n, filter)
	}
	ix, err := e.ensureIndex(ctx, kb)
	if err != nil {
		return nil, err
	}
	hits := ix.search(query, n)
	out := make([]SearchHit, 0, len(hits))
	for _, h := range hits {
		d := ix.doc(h.ID)
		if !filter.Empty() && !metaMatches(d.Meta, filter) {
			continue
		}
		out = append(out, SearchHit{Document: d, Score: float32(h.Score)})
	}
	return out, nil
}

func (e *Engine) bm25Search(ctx context.Context, kb, query string, topK int, filter Filter) ([]SearchHit, error) {
	hits, err := e.keywordHits(ctx, kb, query, topK*4, filter)
	if err != nil {
		return nil, err
	}
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits, nil
}

func (e *Engine) hybridSearchVec(ctx context.Context, kb, query string, vec []float32, topK int, filter Filter) ([]SearchHit, error) {
	candN := topK * 4
	if candN < 20 {
		candN = 20
	}
	sem, err := e.semanticSearchVec(ctx, kb, vec, candN, filter)
	if err != nil {
		return nil, err
	}
	bm, err := e.keywordHits(ctx, kb, query, candN, filter)
	if err != nil {
		bm = nil // keyword side unavailable → degrade to semantic-only
	}
	if len(bm) == 0 {
		if len(sem) > topK {
			sem = sem[:topK]
		}
		return sem, nil
	}

	docByID := make(map[string]Document, len(sem)+len(bm))
	semIDs := make([]string, len(sem))
	for i, h := range sem {
		semIDs[i] = h.ID
		docByID[h.ID] = h.Document
	}
	bmIDs := make([]string, 0, len(bm))
	for _, h := range bm {
		bmIDs = append(bmIDs, h.ID)
		if _, ok := docByID[h.ID]; !ok {
			docByID[h.ID] = h.Document
		}
	}

	fused := rrfFuse([][]string{semIDs, bmIDs},
		[]float64{e.cfg.Pipeline.SemanticWeight, e.cfg.Pipeline.BM25Weight}, topK)
	out := make([]SearchHit, 0, len(fused))
	for rank, id := range fused {
		out = append(out, SearchHit{Document: docByID[id], Score: float32(1.0 / float64(rank+1))})
	}
	return out, nil
}

// docID is a stable UUID for a chunk so re-ingesting the same source
// updates in place rather than duplicating. A deterministic UUIDv5 keeps
// it valid for backends (Qdrant) that require UUID/uint64 point ids.
func docID(kb, source string, chunk int) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("%s\x00%s\x00%d", kb, source, chunk))).String()
}

// Retrieval role constants, mirrored from the embeddings wire so the
// engine doesn't import that package directly.
const (
	pkgmoduleRoleQuery    = "query"
	pkgmoduleRoleDocument = "document"
)

// formatCitations renders retrieved hits into the configured citation
// style for injection alongside the answer context.
func formatCitations(hits []SearchHit, format string) string {
	if len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	for i, h := range hits {
		switch format {
		case "footnote":
			fmt.Fprintf(&b, "[^%d]: %s (chunk %d, score %.3f)\n", i+1, h.Source, h.Chunk, h.Score)
		default: // inline
			fmt.Fprintf(&b, "[%d] %s#%d (%.3f)\n", i+1, h.Source, h.Chunk, h.Score)
		}
	}
	return b.String()
}
