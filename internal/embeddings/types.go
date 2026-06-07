// Package embeddings is the wire contract for the dedicated
// embeddings worker (cmd/digitorn-worker-embeddings). Doc reference :
// docs-site/language/04-tools.md "Semantic search" — the reference
// daemon uses FastEmbed + Qdrant with
// `paraphrase-multilingual-MiniLM-L12-v2` (384 dims).
//
// Architecture :
//
//	daemon          ── gRPC ──>  worker subprocess (this binary)
//	ContextBuilder                ONNX runtime (paraphrase-multilingual-MiniLM-L12-v2)
//	WithEmbeddings                384-dim L2-normalised float32 outputs
//
// The worker pool is managed by internal/worker.Manager. Multiple
// instances run in parallel for throughput ; each one holds the
// model in memory once. Sub-ms p50 batch latency on commodity CPU.
package embeddings

import "time"

// EmbedRequest is the payload the daemon sends to the worker.
//
// Inputs are pre-batched on the daemon side ; the worker iterates
// and returns one vector per input. Tokenisation, padding, and
// mean-pooling all happen in the worker — the daemon never sees
// raw tensors.
type EmbedRequest struct {
	// Inputs is the batch of texts to embed. 1..MaxBatchSize per
	// call. Empty Inputs returns an empty Vectors slice without
	// error.
	Inputs []string `json:"inputs"`

	// Model selects which catalogue model serves the request
	// (canonical id or shortcut, see internal/embeddings/models).
	// Empty resolves to the default model — the historic behaviour,
	// so legacy callers need no change. An unknown id is an error.
	Model string `json:"model,omitempty"`

	// Role hints retrieval intent for models that prepend a prefix
	// (e.g. nomic : "search_query:" / "search_document:"). One of
	// "query", "document", or "" (no prefix). Ignored by models
	// without prefixes.
	Role string `json:"role,omitempty"`

	// Normalize, when true (default), L2-normalises every vector
	// before returning. Required for cosine similarity ; set to
	// false only if the caller needs raw pooled vectors.
	Normalize bool `json:"normalize"`

	// Timeout caps the call. The worker enforces it cooperatively
	// (per-batch checkpoint) ; 0 = no timeout. The gRPC layer
	// also has its own deadline.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// Retrieval roles for EmbedRequest.Role.
const (
	RoleQuery    = "query"
	RoleDocument = "document"
)

// EmbedResponse carries one float32 vector per Input. Vectors are
// always EmbeddingDim long (384 for the doc-default model).
type EmbedResponse struct {
	// Vectors[i] corresponds to req.Inputs[i]. Length always
	// matches len(req.Inputs).
	Vectors [][]float32 `json:"vectors"`

	// Model is the on-disk model identifier the worker used
	// (e.g. "paraphrase-multilingual-MiniLM-L12-v2"). Returned so
	// the daemon can audit version mismatches across a pool.
	Model string `json:"model"`

	// Dimension is the per-vector length the worker emits.
	// Sanity-check for the daemon ; always 384 with the
	// doc-default model.
	Dimension int `json:"dimension"`

	// ElapsedMs is the wall-clock the worker spent in the model
	// (excludes wire time). Used for capacity planning.
	ElapsedMs int64 `json:"elapsed_ms"`
}

// RerankRequest scores documents against a query with a cross-encoder.
type RerankRequest struct {
	Model     string        `json:"model,omitempty"`
	Query     string        `json:"query"`
	Documents []string      `json:"documents"`
	Timeout   time.Duration `json:"timeout,omitempty"`
}

// RerankResponse carries one relevance score per Document (same order).
type RerankResponse struct {
	Scores []float32 `json:"scores"`
	Model  string    `json:"model"`
}

// InfoRequest asks the worker for its loaded model identity.
// Used at boot to verify pool consistency.
type InfoRequest struct{}

// InfoResponse describes the worker's loaded model.
type InfoResponse struct {
	Model     string `json:"model"`
	Dimension int    `json:"dimension"`
	// ReadyAt is the unix nano when the worker finished loading
	// the model. Tells the manager how long startup took.
	ReadyAt int64 `json:"ready_at"`
}

// EmbeddingDim is the doc-mandated vector dimension. The worker
// hard-fails on init if its model emits a different dim — pool
// consistency is non-negotiable.
const EmbeddingDim = 384

// MaxBatchSize caps how many texts one call may carry. Above this
// the daemon must chunk. 256 is the sweet spot for CPU inference
// on the doc-default 384-dim model — beyond that memory pressure
// hurts latency more than batching helps.
const MaxBatchSize = 256

// DefaultModel is the doc-mandated identifier. The worker may
// support others via env override (DIGITORN_EMBED_MODEL) but the
// default must always resolve here.
const DefaultModel = "paraphrase-multilingual-MiniLM-L12-v2"
