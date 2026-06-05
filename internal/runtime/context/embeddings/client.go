// Package embeddings owns the semantic-search half of the
// context_builder's tool index (CB-5). The keyword half lives in
// package index (CB-1) ; this package provides :
//
//   - EmbeddingClient interface : how the daemon obtains an
//     embedding vector for a query or for a tool's index payload.
//   - Vector + cosine similarity primitives.
//   - SemanticIndex : a brute-force in-memory ANN that the
//     tool-search hybrid scorer queries to add a "semantic_score *
//     10" term on top of the keyword score.
//
// Documented in :
//   - docs-site/docs/language/04-tools.md "Semantic search"
//   - docs-site/docs/reference/modules/context_builder.md
//
// The reference daemon uses FastEmbed (paraphrase-multilingual-MiniLM-L12-v2,
// 384 dims) and Qdrant HNSW. CB-5 (V1) :
//
//   - Defines EmbeddingClient as the seam so a real ONNX worker
//     can plug in via gRPC without touching SemanticIndex.
//   - Provides a deterministic DefaultMockClient (hash-based) so
//     tests run without a worker.
//   - Implements brute-force cosine search ; HNSW is a follow-up
//     (lib coder/hnsw) when toolsets cross ~1000 entries.
//
// The ANN structure (brute-force vs HNSW) is encapsulated behind
// SemanticIndex so callers don't care.
package embeddings

import (
	"context"
)

// EmbeddingDim is the documented vector dimension. Locked here so
// every component (mock, worker, index) agrees. Changing this is a
// breaking change across the embeddings boundary.
const EmbeddingDim = 384

// Vector is a fixed-length float32 slice. We don't use a Go array
// [384]float32 so the data flows through Go's normal slice
// machinery (Marshal/Unmarshal, range, append for upsert).
type Vector []float32

// EmbeddingClient computes embeddings for one or more texts. Used
// at build time (to embed every tool's index payload) and at query
// time (to embed the user's search query).
//
// Concurrency : Embed is called from many goroutines at once when
// the builder embeds N tools in parallel. Implementations MUST be
// safe under concurrent calls.
//
// Errors : a transient failure (worker down, timeout) should return
// an error and let the caller fall back to keyword-only search ; a
// permanent failure (wrong dim, malformed input) should also
// return an error and the caller logs.
type EmbeddingClient interface {
	// Embed returns embedding vectors for every input text, in the
	// same order. Returns the same slice length as the input or an
	// error. Each returned Vector has length EmbeddingDim.
	Embed(ctx context.Context, texts []string) ([]Vector, error)
}

// Cosine returns the cosine similarity of two vectors, in [-1, 1].
// Higher = more similar. Both vectors MUST have the same length ;
// a length mismatch returns 0 (treat as "no signal" rather than
// panic, so a malformed input doesn't blow up search).
//
// Pre-normalized vectors (length 1) are NOT assumed — the function
// divides by the L2 norms so callers can pass raw embeddings
// directly. If a caller can pre-normalize (e.g. at index-build
// time), use CosineNormalized for the faster dot-product form.
func Cosine(a, b Vector) float32 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float32
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (sqrt32(na) * sqrt32(nb))
}

// CosineNormalized is the fast path when both vectors are already
// L2-normalized (length 1). Returns the plain dot product, which
// equals cosine similarity in that case.
func CosineNormalized(a, b Vector) float32 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot float32
	for i := range a {
		dot += a[i] * b[i]
	}
	return dot
}

// Normalize divides every component by the L2 norm so the vector
// has length 1. Returns the input slice mutated in place ; callers
// who can't mutate should copy first.
func Normalize(v Vector) Vector {
	if len(v) == 0 {
		return v
	}
	var norm float32
	for _, x := range v {
		norm += x * x
	}
	if norm == 0 {
		return v
	}
	inv := 1 / sqrt32(norm)
	for i := range v {
		v[i] *= inv
	}
	return v
}

// sqrt32 is the float32 square root. Standalone to keep the
// math package out of the hot-path imports.
func sqrt32(x float32) float32 {
	if x <= 0 {
		return 0
	}
	// Newton-Raphson with a single iteration is plenty for our
	// precision needs (cosine similarity below 6 decimal places
	// doesn't change ranking).
	y := x
	z := x / 2
	for i := 0; i < 8 && z != y; i++ {
		y = z
		z = (y + x/y) / 2
	}
	return z
}
