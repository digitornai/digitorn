package embeddings

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"

	ctxembed "github.com/mbathepaul/digitorn/internal/runtime/context/embeddings"
	"github.com/mbathepaul/digitorn/internal/worker"
)

// Client is the daemon-side handle to the embeddings worker pool.
// It satisfies ctxembed.EmbeddingClient so wiring.New(actions).
// WithEmbeddings(client) can plug it directly into the
// context_builder pipeline.
//
// Pool routing : each call picks one healthy worker via the shared
// worker.Manager (round-robin / least-loaded — Manager decides).
// Calls are independent ; a slow inference on instance A doesn't
// block instance B. If no worker is ready, the call returns an
// error and the caller falls back to keyword-only search.
//
// Batching : the Client transparently chunks any input slice
// larger than MaxBatchSize into multiple worker calls and
// concatenates the results.
type Client struct {
	mgr *worker.Manager
	// kind defaults to the package's Kind constant ; tests can
	// override.
	kind worker.Kind
	// timeout caps each underlying RPC. 0 = no per-call deadline
	// (the worker still enforces its own). Default 10s.
	timeout time.Duration
}

// NewClient constructs a Client over the given Manager. The
// caller is responsible for having registered an embeddings Spec
// on the Manager beforehand (see bootstrap.go).
func NewClient(mgr *worker.Manager) *Client {
	return &Client{mgr: mgr, kind: Kind, timeout: 10 * time.Second}
}

// WithTimeout sets the per-RPC timeout. Returns the receiver for
// chaining. 0 disables the deadline (worker still enforces its
// own EmbedRequest.Timeout).
func (c *Client) WithTimeout(d time.Duration) *Client {
	c.timeout = d
	return c
}

// Embed implements ctxembed.EmbeddingClient. Forwards the texts
// to a healthy worker, chunks if necessary, and returns the
// concatenated vectors in input order.
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

// embedChunk handles a single batch ≤ MaxBatchSize. Returns the
// vectors verbatim (Normalize=true so they're cosine-ready).
func (c *Client) embedChunk(ctx context.Context, texts []string) ([]ctxembed.Vector, error) {
	if c.mgr == nil {
		return nil, errors.New("embeddings: no worker manager")
	}
	conn, err := c.mgr.Pick(ctx, c.kind)
	if err != nil {
		return nil, fmt.Errorf("embeddings: pick worker: %w", err)
	}

	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	req := &EmbedRequest{Inputs: texts, Normalize: true}
	var resp EmbedResponse
	err = conn.GRPC().Invoke(ctx,
		"/"+ServiceName+"/"+MethodEmbed,
		req, &resp,
		grpc.CallContentSubtype(CodecName),
	)
	if err != nil {
		return nil, fmt.Errorf("embeddings: rpc: %w", err)
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

// Compile-time guard : *Client must satisfy ctxembed.EmbeddingClient.
var _ ctxembed.EmbeddingClient = (*Client)(nil)
