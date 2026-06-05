package embeddings

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
)

// MockClient is a deterministic EmbeddingClient suitable for tests
// and offline development. It produces a 384-dimensional vector
// derived from the input text via FNV-1a hashing + sinusoidal
// projection.
//
// Properties :
//
//   - Deterministic : same input → same output every time.
//   - Similar inputs → similar outputs : tokens shared between two
//     strings push their vectors toward each other so cosine
//     similarity captures lexical overlap (NOT semantic overlap —
//     "delete" and "remove" stay orthogonal unless they share
//     tokens). This is enough to verify the SemanticIndex wiring ;
//     real semantic matches require the ONNX worker.
//   - L2-normalized : every output has length 1.
//
// Not suitable for production. The real semantic search uses
// FastEmbed paraphrase-multilingual-MiniLM-L12-v2 in a worker
// process (gRPC).
type MockClient struct{}

// Embed implements EmbeddingClient. Always returns nil error.
func (MockClient) Embed(_ context.Context, texts []string) ([]Vector, error) {
	out := make([]Vector, len(texts))
	for i, t := range texts {
		out[i] = mockEmbed(t)
	}
	return out, nil
}

// mockEmbed hashes the lower-cased tokens of `text` into a stable
// 384-dim vector. Tokens that share characters produce overlapping
// vector contributions, so cosine similarity is meaningful between
// strings with shared vocabulary.
func mockEmbed(text string) Vector {
	tokens := strings.Fields(strings.ToLower(text))
	v := make(Vector, EmbeddingDim)
	for _, tok := range tokens {
		h := fnv.New64a()
		_, _ = h.Write([]byte(tok))
		seed := h.Sum64()
		for i := 0; i < EmbeddingDim; i++ {
			// Each token contributes a phase-shifted sine across
			// every dimension. Different tokens land at different
			// phases ; shared tokens push the same dimensions.
			phase := float64(seed) + float64(i)*0.137
			v[i] += float32(math.Sin(phase))
		}
	}
	return Normalize(v)
}
