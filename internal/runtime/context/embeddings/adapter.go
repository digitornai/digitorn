package embeddings

import (
	"context"

	"github.com/digitornai/digitorn/internal/runtime/context/index"
)

type Adapter struct {
	Index  *SemanticIndex
	Client EmbeddingClient
}

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

func Attach(idx *index.ToolIndex, semIdx *SemanticIndex, client EmbeddingClient) {
	if idx == nil {
		return
	}
	idx.Semantic = &Adapter{Index: semIdx, Client: client}
}

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
