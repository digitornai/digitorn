package embeddings

import (
	"context"
	"sort"
)

// SemanticIndex stores one normalized vector per tool FQN and
// answers "top-K most similar tools to this query" via brute-force
// cosine similarity.
//
// V1 implementation : a flat array, O(N) per query. Fine for
// catalogs up to ~5000 tools — a 5000-tool query at dim 384 is
// ~2M float multiplications, sub-millisecond on commodity CPU.
// Beyond that, swap in HNSW (lib coder/hnsw) ; the public surface
// stays the same so callers don't change.
//
// Thread-safety : SemanticIndex is read-only after construction.
// Many goroutines can call Search concurrently without locks.
type SemanticIndex struct {
	vectors []indexedVector
	dim     int
}

type indexedVector struct {
	FQN string
	Vec Vector
}

// SemanticHit is one similarity search result.
type SemanticHit struct {
	FQN   string
	Score float32 // cosine similarity, in [-1, 1]
}

// NewSemanticIndex builds a SemanticIndex by embedding the corpus
// of every IndexedTool. `corpus` maps FQN → the text payload to
// embed (typically "description + tags + aliases" concatenated).
//
// Embeddings are batched in a single Embed call so a real worker
// can amortize network overhead. On failure, returns the error and
// no partial index — callers fall back to keyword-only search.
//
// Vectors are normalized at build time so Search uses the fast
// CosineNormalized dot product.
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
	// Re-sort texts in the same order so vec[i] corresponds to fqns[i].
	// Cheaper than maintaining a parallel sort key : we rebuild texts
	// by iterating the sorted fqns.
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

// Search returns the top-`limit` most-similar FQNs to `queryVec`.
// queryVec doesn't need to be normalized — Search normalizes a
// local copy. limit <= 0 returns every tool.
func (s *SemanticIndex) Search(queryVec Vector, limit int) []SemanticHit {
	if s == nil || len(s.vectors) == 0 || len(queryVec) == 0 {
		return nil
	}
	// Normalize a copy so we don't mutate the caller's slice.
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

// Size returns the number of indexed vectors.
func (s *SemanticIndex) Size() int {
	if s == nil {
		return 0
	}
	return len(s.vectors)
}

// errMismatchedVectorCount is returned when the EmbeddingClient
// gives back a different number of vectors than texts passed in.
// Stays as a package-level value (vs fmt.Errorf) so tests can
// errors.Is against it if a caller chooses to handle the case.
var errMismatchedVectorCount = embeddingError("embedding client returned mismatched vector count")

type embeddingError string

func (e embeddingError) Error() string { return string(e) }
