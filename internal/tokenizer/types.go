// Package tokenizer is the wire contract for the dedicated tokenizer worker
// (cmd/digitorn-worker-tokenizer). It exists so token counting — CPU-bound and
// potentially heavy at scale — runs OUT of the daemon process : a crash or a
// saturated tokenizer pool can never slow the turn loop, and the pool scales
// independently of the I/O-bound LLM pool.
//
//	daemon                ── gRPC (json) ──>  worker subprocess (this binary)
//	ContextService                            tokencount.Counter (tiktoken + heuristic)
//	(background delta refine)                 content-addressed immutable cache
//
// The provider usage anchor (CTX-7.1) already gives the EXACT count at every
// turn boundary for free, so this worker only refines the between-anchor delta.
// If the pool is down the daemon simply keeps the anchor value — graceful
// degradation, never a block.
package tokenizer

import "time"

// CountRequest asks the worker to count tokens for a batch of texts under one
// model. Model is constant per call (a turn uses one agent brain) ; the worker
// routes Model → tiktoken encoding, falling back to the heuristic.
type CountRequest struct {
	Texts    []string      `json:"texts"`
	Provider string        `json:"provider,omitempty"`
	Model    string        `json:"model,omitempty"`
	Timeout  time.Duration `json:"timeout,omitempty"`
}

// CountResponse returns one count per input text plus their sum. Counts[i]
// corresponds to req.Texts[i].
type CountResponse struct {
	Counts    []int `json:"counts"`
	Total     int   `json:"total"`
	ElapsedMs int64 `json:"elapsed_ms"`
}

// InfoRequest asks the worker for liveness/identity.
type InfoRequest struct{}

// InfoResponse describes the worker.
type InfoResponse struct {
	ReadyAt int64 `json:"ready_at"`
}

// MaxBatchSize caps how many texts one call may carry ; the daemon chunks
// above this. A delta is a handful of messages, so this is comfortably high.
const MaxBatchSize = 1024
