package embeddings

import (
	"context"

	"github.com/mbathepaul/digitorn/internal/runtime/context/index"
)

// Adapter wraps a SemanticIndex + EmbeddingClient to implement the
// index.SemanticSearcher interface declared by package index. This
// indirection avoids a circular import between index (keyword
// search) and embeddings (semantic search) while still letting the
// keyword search consume semantic hits when present.
//
// Use Attach to wire a SemanticIndex onto a ToolIndex once both
// are built :
//
//	idx := indexBuilder.Build(...)
//	semIdx, _ := embeddings.NewSemanticIndex(ctx, client, corpus)
//	embeddings.Attach(idx, semIdx, client)
type Adapter struct {
	Index  *SemanticIndex
	Client EmbeddingClient
}

// SearchVector forwards to SemanticIndex.Search and converts hits
// to the index-package type.
func (a *Adapter) SearchVector(queryVec []float32, limit int) []index.SemanticHit {
	if a == nil || a.Index == nil {
		return nil
	}
	hits := a.Index.Search(Vector(queryVec), limit)
	out := make([]index.SemanticHit, len(hits))
	for i, h := range hits {
		out[i] = index.SemanticHit{FQN: h.FQN, Score: h.Score}
	}
	return out
}

// EmbedQuery embeds a single query string and returns the vector
// as []float32 (the index package doesn't import this package's
// Vector type).
func (a *Adapter) EmbedQuery(query string) ([]float32, error) {
	if a == nil || a.Client == nil {
		return nil, nil
	}
	vecs, err := a.Client.Embed(context.Background(), []string{query})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, nil
	}
	return []float32(vecs[0]), nil
}

// Attach plugs the semantic index + client onto an existing
// ToolIndex so future Search calls use hybrid scoring. ToolIndex
// stays immutable after Attach — the Semantic field is the only
// post-construction mutation, and it's a single atomic pointer
// write that any goroutine doing a Search safely picks up via the
// Go memory model when Attach completes before Search starts (the
// typical wiring : daemon builds index → Attach → publishes to
// engine).
func Attach(idx *index.ToolIndex, semIdx *SemanticIndex, client EmbeddingClient) {
	if idx == nil {
		return
	}
	idx.Semantic = &Adapter{Index: semIdx, Client: client}
}

// BuildCorpus assembles the per-tool text payload that
// NewSemanticIndex embeds. Concatenates the fields the doc lists
// as the semantic corpus :
//
//	"FQN + description + tags + parameter names + side effects +
//	 aliases + synonym expansion"
//
// Synonym expansion is the keyword-side concern ; we don't include
// it here because the semantic model handles language variation
// natively (multilingual paraphrase-MiniLM in production).
//
// The returned map is suitable to pass to NewSemanticIndex.
func BuildCorpus(tools map[string]*index.IndexedTool) map[string]string {
	out := make(map[string]string, len(tools))
	for fqn, t := range tools {
		out[fqn] = corpusFor(t)
	}
	return out
}

func corpusFor(t *index.IndexedTool) string {
	if t == nil {
		return ""
	}
	parts := make([]string, 0, 8)
	parts = append(parts, t.FQN, t.Description)
	parts = append(parts, t.Tags...)
	parts = append(parts, t.Aliases...)
	for _, p := range t.Params {
		parts = append(parts, p.Name)
		if p.Description != "" {
			parts = append(parts, p.Description)
		}
	}
	return joinNonEmpty(parts, " ")
}

func joinNonEmpty(parts []string, sep string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += sep
		}
		out += p
	}
	return out
}
