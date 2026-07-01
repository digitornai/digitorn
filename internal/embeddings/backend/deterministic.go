package backend

import (
	"context"
	"hash/fnv"
	"math"
	"strings"

	"github.com/digitornai/digitorn/internal/embeddings/models"
)

// Deterministic is the fallback inference backend : it produces
// stable 384-dim vectors using a hashing scheme. It does NOT do
// real semantic matching — two synonymous queries get unrelated
// vectors. But it preserves the contract (length, normalisation,
// reproducibility) so the rest of the runtime can be tested and
// the daemon can boot without a downloaded model.
//
// Useful for :
//   - CI runs where downloading the ONNX model adds 90s
//   - Cross-compile builds where ONNX-CGO is not available
//     (mobile, WASM) — the worker stays callable but degrades to
//     keyword-only effective behaviour (semantic hits are
//     basically random)
//   - Local smoke tests without disk space for the model
//
// Production deployments MUST use the ONNX backend (build with
// `-tags onnx`).
type Deterministic struct {
	dim  int
	name string
}

// NewDeterministic constructs the fallback backend. dim should
// always be 384 to match the doc-mandated EmbeddingDim ; an
// override is exposed for tests.
func NewDeterministic(dim int) *Deterministic {
	if dim <= 0 {
		dim = 384
	}
	return &Deterministic{dim: dim, name: "deterministic-hash-v1"}
}

// NewDeterministicFor builds a fallback backend matching a model Spec's
// dimension and echoing its id, so a no-ONNX build still serves the
// requested model's shape (vectors are not semantic — fallback only).
func NewDeterministicFor(spec models.Spec) *Deterministic {
	dim := spec.Dim
	if dim <= 0 {
		dim = 384
	}
	name := spec.ID
	if name == "" {
		name = "deterministic-hash-v1"
	}
	return &Deterministic{dim: dim, name: name}
}

func (d *Deterministic) Model() string  { return d.name }
func (d *Deterministic) Dimension() int { return d.dim }
func (d *Deterministic) Close() error   { return nil }

// Embed computes per-input vectors. Token-bag : each space-split
// token contributes to D coordinates determined by FNV hashes of
// the token. Same input → same vector (reproducible). Different
// inputs sharing tokens get correlated vectors, so the property
// "swap two words and the cosine stays high" still holds — good
// enough for sanity tests that only need monotonic behaviour.
func (d *Deterministic) Embed(_ context.Context, inputs []string, l2norm bool) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i, text := range inputs {
		vec := make([]float32, d.dim)
		text = strings.ToLower(strings.TrimSpace(text))
		for _, tok := range strings.Fields(text) {
			for j := 0; j < 4; j++ { // 4 hash projections per token
				h := fnv.New32a()
				h.Write([]byte(tok))
				h.Write([]byte{byte(j)})
				slot := int(h.Sum32()) % d.dim
				if slot < 0 {
					slot += d.dim
				}
				// Sign from another hash so contributions can cancel.
				sh := fnv.New32a()
				sh.Write([]byte(tok))
				sh.Write([]byte{byte(j ^ 0x80)})
				if sh.Sum32()%2 == 0 {
					vec[slot] += 1
				} else {
					vec[slot] -= 1
				}
			}
		}
		if l2norm {
			normalize(vec)
		}
		out[i] = vec
	}
	return out, nil
}

// deterministicCrossEncoder is the no-op reranker fallback : it preserves
// the input order (descending scores) so retrieval still works without a
// real cross-encoder. NOT a real relevance model.
type deterministicCrossEncoder struct{ name string }

// NewDeterministicCrossEncoder builds the fallback reranker for a Spec.
func NewDeterministicCrossEncoder(spec models.Spec) CrossEncoder {
	name := spec.ID
	if name == "" {
		name = "deterministic-rerank"
	}
	return &deterministicCrossEncoder{name: name}
}

func (d *deterministicCrossEncoder) Model() string { return d.name }
func (d *deterministicCrossEncoder) Close() error  { return nil }
func (d *deterministicCrossEncoder) Rerank(_ context.Context, _ string, docs []string) ([]float32, error) {
	out := make([]float32, len(docs))
	for i := range docs {
		out[i] = float32(len(docs) - i)
	}
	return out, nil
}

// normalize divides v by its L2 norm in-place. No-op on a
// zero-vector (which happens for empty input).
func normalize(v []float32) {
	var sumSq float64
	for _, x := range v {
		sumSq += float64(x) * float64(x)
	}
	if sumSq == 0 {
		return
	}
	norm := float32(math.Sqrt(sumSq))
	for i := range v {
		v[i] /= norm
	}
}
