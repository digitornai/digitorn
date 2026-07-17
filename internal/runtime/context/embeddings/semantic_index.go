package embeddings

import (
	"context"
	"sort"
)

type SemanticIndex struct {
	vectors []indexedVector
	dim     int
}

type indexedVector struct {
	FQN string
	Vec Vector
}

type SemanticHit struct {
	FQN   string
	Score float32
}

func NewSemanticIndex(ctx context.Context, client EmbeddingClient, corpus map[string]string) (*SemanticIndex, error) {
	if client == nil || len(corpus) == 0 {
		return &SemanticIndex{}, nil
	}
	fqns := make([]string, 0, len(corpus))
	texts := make([]string, 0, len(corpus))
	for fqn, payload := range corpus {
		fqns = append(fqns, fqn)
		texts = append(texts, payload)
	}
	sort.Slice(fqns, func(i, j int) bool { return fqns[i] < fqns[j] })
	texts = texts[:0]
	for _, fqn := range fqns {
		texts = append(texts, corpus[fqn])
	}

	vecs, err := client.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	if len(vecs) != len(fqns) {
		return nil, errMismatchedVectorCount
	}

	idx := &SemanticIndex{
		vectors: make([]indexedVector, 0, len(fqns)),
		dim:     EmbeddingDim,
	}
	for i, v := range vecs {
		if len(v) == 0 {
			continue
		}
		Normalize(v)
		idx.vectors = append(idx.vectors, indexedVector{FQN: fqns[i], Vec: v})
	}
	return idx, nil
}

func (s *SemanticIndex) Search(queryVec Vector, limit int) []SemanticHit {
	if s == nil || len(s.vectors) == 0 || len(queryVec) == 0 {
		return nil
	}
	q := make(Vector, len(queryVec))
	copy(q, queryVec)
	Normalize(q)

	hits := make([]SemanticHit, 0, len(s.vectors))
	for _, iv := range s.vectors {
		hits = append(hits, SemanticHit{
			FQN:   iv.FQN,
			Score: CosineNormalized(q, iv.Vec),
		})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].FQN < hits[j].FQN
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

func (s *SemanticIndex) Size() int {
	if s == nil {
		return 0
	}
	return len(s.vectors)
}

var errMismatchedVectorCount = embeddingError("embedding client returned mismatched vector count")

type embeddingError string

func (e embeddingError) Error() string { return string(e) }
