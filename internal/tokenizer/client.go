package tokenizer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"

	"github.com/digitornai/digitorn/internal/worker"
)

// Client is the daemon-side handle to the tokenizer worker pool. Each call
// picks one healthy worker via the shared worker.Manager (round-robin) ; calls
// are independent so a slow count on instance A never blocks instance B. If no
// worker is ready the call returns an error and the caller keeps the provider
// anchor value — graceful degradation, never a block on the turn loop.
type Client struct {
	mgr     *worker.Manager
	kind    worker.Kind
	timeout time.Duration
}

// NewClient constructs a Client over the given Manager. The caller registers a
// tokenizer Spec on the Manager beforehand (see bootstrap).
func NewClient(mgr *worker.Manager) *Client {
	return &Client{mgr: mgr, kind: Kind, timeout: 5 * time.Second}
}

// WithTimeout sets the per-RPC timeout (0 disables the deadline).
func (c *Client) WithTimeout(d time.Duration) *Client {
	c.timeout = d
	return c
}

// CountTotal returns the total token count of texts under provider/model,
// chunking above MaxBatchSize. Empty input is 0 with no RPC.
func (c *Client) CountTotal(ctx context.Context, texts []string, provider, model string) (int, error) {
	if len(texts) == 0 {
		return 0, nil
	}
	if c == nil || c.mgr == nil {
		return 0, errors.New("tokenizer: no worker manager")
	}
	total := 0
	for start := 0; start < len(texts); start += MaxBatchSize {
		end := start + MaxBatchSize
		if end > len(texts) {
			end = len(texts)
		}
		n, err := c.countChunk(ctx, texts[start:end], provider, model)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// CountEach returns the per-text token counts in the SAME order as texts,
// chunking above MaxBatchSize. It lets a caller count several buckets in one
// pass and split the result by bucket boundaries, instead of one RPC per bucket.
// Empty input → nil, no RPC.
func (c *Client) CountEach(ctx context.Context, texts []string, provider, model string) ([]int, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if c == nil || c.mgr == nil {
		return nil, errors.New("tokenizer: no worker manager")
	}
	out := make([]int, 0, len(texts))
	for start := 0; start < len(texts); start += MaxBatchSize {
		end := min(start+MaxBatchSize, len(texts))
		counts, err := c.countChunkEach(ctx, texts[start:end], provider, model)
		if err != nil {
			return nil, err
		}
		out = append(out, counts...)
	}
	return out, nil
}

func (c *Client) countChunkEach(ctx context.Context, texts []string, provider, model string) ([]int, error) {
	conn, err := c.mgr.Pick(ctx, c.kind)
	if err != nil {
		return nil, fmt.Errorf("tokenizer: pick worker: %w", err)
	}
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	req := &CountRequest{Texts: texts, Provider: provider, Model: model}
	var resp CountResponse
	if err := conn.GRPC().Invoke(ctx,
		"/"+ServiceName+"/"+MethodCount,
		req, &resp,
		grpc.CallContentSubtype(CodecName),
	); err != nil {
		return nil, fmt.Errorf("tokenizer: rpc: %w", err)
	}
	if len(resp.Counts) != len(texts) {
		return nil, fmt.Errorf("tokenizer: per-text count mismatch (got %d, want %d)", len(resp.Counts), len(texts))
	}
	return resp.Counts, nil
}

func (c *Client) countChunk(ctx context.Context, texts []string, provider, model string) (int, error) {
	conn, err := c.mgr.Pick(ctx, c.kind)
	if err != nil {
		return 0, fmt.Errorf("tokenizer: pick worker: %w", err)
	}
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	req := &CountRequest{Texts: texts, Provider: provider, Model: model}
	var resp CountResponse
	if err := conn.GRPC().Invoke(ctx,
		"/"+ServiceName+"/"+MethodCount,
		req, &resp,
		grpc.CallContentSubtype(CodecName),
	); err != nil {
		return 0, fmt.Errorf("tokenizer: rpc: %w", err)
	}
	return resp.Total, nil
}
