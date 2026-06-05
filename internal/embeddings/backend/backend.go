// Package backend defines the inference interface the embeddings
// worker plugs into. Two implementations ship :
//
//   - deterministic : pure-Go hashing backend with 384-dim
//     output. Always available, used as a fallback when no real
//     model is loaded AND in CI where the ONNX model isn't
//     downloaded.
//   - onnx (build-tag) : yalue/onnxruntime_go binding loading
//     paraphrase-multilingual-MiniLM-L12-v2. Production path.
//
// Choosing between them happens at worker startup based on env :
// DIGITORN_EMBED_BACKEND=onnx|deterministic (default onnx ; falls
// back to deterministic if the ONNX runtime fails to init).
package backend

import "context"

// Backend is the inference contract.
type Backend interface {
	// Embed returns one [EmbeddingDim]float32 per input text.
	// Vectors are L2-normalised when normalize=true. The slice
	// returned has the same len as inputs.
	Embed(ctx context.Context, inputs []string, normalize bool) ([][]float32, error)

	// Model returns the loaded model identifier
	// (paraphrase-multilingual-MiniLM-L12-v2 for the doc default,
	// or "deterministic-hash-v1" for the fallback).
	Model() string

	// Dimension returns the per-vector length. Doc-default 384.
	Dimension() int

	// Close releases any heavy resources (ONNX session, mmap,
	// tensors). Idempotent.
	Close() error
}
