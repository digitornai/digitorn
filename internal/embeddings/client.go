package embeddings

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"

	ctxembed "github.com/digitornai/digitorn/internal/runtime/context/embeddings"
	"github.com/digitornai/digitorn/internal/worker"
)

// Client is a handle to the embeddings service. Two construction paths :
//
//   - NewClient(mgr) : daemon-side, routes each call through the shared
//     worker.Manager (picks one healthy embeddings worker). Satisfies
//     ctxembed.EmbeddingClient so it plugs into the context_builder.
//   - NewDirectClient(cc) : over a fixed gRPC connection — used by a
//     worker-hosted module talking to the daemon service gateway, where
//     there is no local worker.Manager to pick from.
//
// Batching : input slices larger than MaxBatchSize are split into
// multiple calls and concatenated.
type Client struct {
	// pick yields the gRPC connection for one call. Abstracts the
	// Manager (daemon) vs fixed-conn (worker→gateway) transports.
	pick func(context.Context) (grpc.ClientConnInterface, error)
	// timeout caps each underlying RPC. 0 = no per-call deadline
	// (the worker still enforces its own).
	timeout time.Duration
}

// NewClient constructs a Client over the given Manager. The caller is
// responsible for having registered an embeddings Spec on the Manager
// beforehand (see bootstrap.go).
func NewClient(mgr *worker.Manager) *Client {
	return &Client{
		pick: func(ctx context.Context) (grpc.ClientConnInterface, error) {
			conn, err := mgr.Pick(ctx, Kind)
			if err != nil {
				return nil, err
			}
			return conn.GRPC(), nil
		},
		timeout: 10 * time.Second,
	}
}

// NewDirectClient builds a Client over a fixed gRPC connection. Used by
// worker-hosted modules (RAG) that reach the embeddings service through
// the daemon service gateway.
func NewDirectClient(cc grpc.ClientConnInterface) *Client {
	return &Client{
		pick:    func(context.Context) (grpc.ClientConnInterface, error) { return cc, nil },
		timeout: 30 * time.Second,
	}
}

// WithTimeout sets the per-RPC timeout. Returns the receiver for
// chaining. 0 disables the deadline (worker still enforces its own).
func (c *Client) WithTimeout(d time.Duration) *Client {
	c.timeout = d
	return c
}

// Embed implements ctxembed.EmbeddingClient : default model, 384-dim,
// cosine-ready vectors. Legacy callers (semantic search) are unchanged.
func (c *Client) Embed(ctx context.Context, texts []string) ([]ctxembed.Vector, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([]ctxembed.Vector, 0, len(texts))
	for start := 0; start < len(texts); start += MaxBatchSize {
		end := start + MaxBatchSize
		if end > len(texts) {
			end = len(texts)
		}
		chunk := texts[start:end]
		vecs, err := c.embedChunk(ctx, chunk)
		if err != nil {
			return nil, err
		}
		if len(vecs) != len(chunk) {
			return nil, fmt.Errorf("embeddings: worker returned %d vectors for %d inputs",
				len(vecs), len(chunk))
		}
		out = append(out, vecs...)
	}
	return out, nil
}

// EmbedModel embeds texts with a specific model + retrieval role,
// returning vectors of that model's dimension (NOT 384-locked) plus the
// resolved dimension. Empty model = default. Used by the RAG module.
func (c *Client) EmbedModel(ctx context.Context, model, role string, texts []string) ([]ctxembed.Vector, int, error) {
	if len(texts) == 0 {
		return nil, 0, nil
	}
	out := make([]ctxembed.Vector, 0, len(texts))
	dim := 0
	for start := 0; start < len(texts); start += MaxBatchSize {
		end := start + MaxBatchSize
		if end > len(texts) {
			end = len(texts)
		}
		chunk := texts[start:end]
		resp, err := c.invoke(ctx, &EmbedRequest{Inputs: chunk, Model: model, Role: role, Normalize: true})
		if err != nil {
			return nil, 0, err
		}
		if len(resp.Vectors) != len(chunk) {
			return nil, 0, fmt.Errorf("embeddings: worker returned %d vectors for %d inputs",
				len(resp.Vectors), len(chunk))
		}
		dim = resp.Dimension
		for _, v := range resp.Vectors {
			out = append(out, ctxembed.Vector(v))
		}
	}
	return out, dim, nil
}

// EmbedRaw forwards a full request and returns the worker's full
// response (model + dimension + vectors). Used by the service gateway
// to relay requests verbatim.
func (c *Client) EmbedRaw(ctx context.Context, req *EmbedRequest) (*EmbedResponse, error) {
	return c.invoke(ctx, req)
}

// Rerank scores docs against query with a cross-encoder, returning one
// score per doc. Empty model = default reranker. Used by the RAG module.
func (c *Client) Rerank(ctx context.Context, model, query string, docs []string) ([]float32, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	resp, err := c.RerankRaw(ctx, &RerankRequest{Model: model, Query: query, Documents: docs})
	if err != nil {
		return nil, err
	}
	return resp.Scores, nil
}

// RerankRaw forwards a full rerank request and returns the worker's full
// response. Used by the service gateway to relay verbatim.
func (c *Client) RerankRaw(ctx context.Context, req *RerankRequest) (*RerankResponse, error) {
	if c.pick == nil {
		return nil, errors.New("embeddings: no connection source")
	}
	cc, err := c.pick(ctx)
	if err != nil {
		return nil, fmt.Errorf("embeddings: pick worker: %w", err)
	}
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	var resp RerankResponse
	if err := cc.Invoke(ctx,
		"/"+ServiceName+"/"+MethodRerank,
		req, &resp,
		grpc.CallContentSubtype(CodecName),
	); err != nil {
		return nil, fmt.Errorf("embeddings: rerank rpc: %w", err)
	}
	return &resp, nil
}

// embedChunk handles a single batch ≤ MaxBatchSize for the legacy
// 384-dim path (Normalize=true so vectors are cosine-ready).
func (c *Client) embedChunk(ctx context.Context, texts []string) ([]ctxembed.Vector, error) {
	resp, err := c.invoke(ctx, &EmbedRequest{Inputs: texts, Normalize: true})
	if err != nil {
		return nil, err
	}
	if resp.Dimension != 0 && resp.Dimension != EmbeddingDim {
		return nil, fmt.Errorf("embeddings: worker returned dim=%d, want %d",
			resp.Dimension, EmbeddingDim)
	}
	out := make([]ctxembed.Vector, len(resp.Vectors))
	for i, v := range resp.Vectors {
		if len(v) != EmbeddingDim {
			return nil, fmt.Errorf("embeddings: vector[%d] has %d dims, want %d",
				i, len(v), EmbeddingDim)
		}
		out[i] = ctxembed.Vector(v)
	}
	return out, nil
}

// invoke picks a connection and calls the worker's Embed method,
// returning the full response.
func (c *Client) invoke(ctx context.Context, req *EmbedRequest) (*EmbedResponse, error) {
	if c.pick == nil {
		return nil, errors.New("embeddings: no connection source")
	}
	cc, err := c.pick(ctx)
	if err != nil {
		return nil, fmt.Errorf("embeddings: pick worker: %w", err)
	}
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	var resp EmbedResponse
	if err := cc.Invoke(ctx,
		"/"+ServiceName+"/"+MethodEmbed,
		req, &resp,
		grpc.CallContentSubtype(CodecName),
	); err != nil {
		return nil, fmt.Errorf("embeddings: rpc: %w", err)
	}
	return &resp, nil
}

// Compile-time guard : *Client must satisfy ctxembed.EmbeddingClient.
var _ ctxembed.EmbeddingClient = (*Client)(nil)
