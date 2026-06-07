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

	mu       sync.Mutex
	idx      map[string]*kbIndex          // kb -> keyword index (BM25 + doc metadata)
	srcState map[string]map[string]string // sourceKey -> relpath -> stat-sig (incremental sync)
}

// kbIndex is the in-memory keyword side of a knowledge base : a BM25
// index plus the chunk metadata needed to resolve a hit back to a
// citation. Rebuilt from the vector store on cold start.
type kbIndex struct {
	bm25 *BM25
	docs map[string]Document
}

func (ix *kbIndex) add(d Document) {
	ix.bm25.Add(d.ID, d.Text)
	ix.docs[d.ID] = Document{ID: d.ID, Text: d.Text, Source: d.Source, Chunk: d.Chunk}
}

// NewEngine wires an engine from a parsed Config, a backend connector,
// the embedder and (optionally) the reranker bridged in from the daemon.
func NewEngine(cfg Config, backend VectorBackend, embed pkgmodule.Embedder, reranker pkgmodule.Reranker) *Engine {
	return &Engine{
		cfg: cfg, backend: backend, embed: embed, reranker: reranker, model: cfg.EmbeddingModel.ID,
		idx:      map[string]*kbIndex{},
		srcState: map[string]map[string]string{},
	}
}

// Ingest chunks text, embeds the chunks as documents, ensures the KB
// exists at the right dimension, and upserts. Returns the chunk count.
func (e *Engine) Ingest(ctx context.Context, kb, text, source string) (int, error) {
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
	docs := make([]Document, len(chunks))
	for i, c := range chunks {
		docs[i] = Document{
			ID:     docID(kb, source, c.Index),
			Vector: vecs[i],
			Text:   c.Text,
			Source: source,
			Chunk:  c.Index,
		}
	}
	if err := e.backend.Upsert(ctx, kb, docs); err != nil {
		return 0, fmt.Errorf("rag: upsert: %w", err)
	}
	ix := e.indexFor(kb)
	for _, d := range docs {
		ix.add(d)
	}
	return len(docs), nil
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
	if ix.bm25.Len() > 0 {
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

	var hits []SearchHit
	var err error
	switch strings.ToLower(e.cfg.Pipeline.Retrieval) {
	case "bm25":
		hits, err = e.bm25Search(ctx, kb, query, retrieveK)
	case "semantic":
		hits, err = e.semanticSearch(ctx, kb, query, retrieveK)
	default:
		hits, err = e.hybridSearch(ctx, kb, query, retrieveK)
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

func (e *Engine) semanticSearch(ctx context.Context, kb, query string, topK int) ([]SearchHit, error) {
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
	return e.backend.Search(ctx, kb, vecs[0], topK)
}

func (e *Engine) bm25Search(ctx context.Context, kb, query string, topK int) ([]SearchHit, error) {
	ix, err := e.ensureIndex(ctx, kb)
	if err != nil {
		return nil, err
	}
	hits := ix.bm25.Search(query, topK)
	out := make([]SearchHit, 0, len(hits))
	for _, h := range hits {
		d := ix.docs[h.ID]
		out = append(out, SearchHit{Document: d, Score: float32(h.Score)})
	}
	return out, nil
}

func (e *Engine) hybridSearch(ctx context.Context, kb, query string, topK int) ([]SearchHit, error) {
	candN := topK * 4
	if candN < 20 {
		candN = 20
	}
	sem, err := e.semanticSearch(ctx, kb, query, candN)
	if err != nil {
		return nil, err
	}
	ix, err := e.ensureIndex(ctx, kb)
	if err != nil {
		return nil, err
	}
	bm := ix.bm25.Search(query, candN)
	if len(bm) == 0 {
		// No keyword side (empty index) — semantic-only, trimmed to topK.
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
	bmIDs := make([]string, len(bm))
	for i, h := range bm {
		bmIDs[i] = h.ID
		if _, ok := docByID[h.ID]; !ok {
			docByID[h.ID] = ix.docs[h.ID]
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
