package tokenizer

import (
	"context"
	"fmt"
	"time"

	"github.com/mbathepaul/digitorn/internal/runtime/tokencount"
)

// Server is the production Service implementation : it wraps a tokencount
// Counter (tiktoken + heuristic + content-addressed cache) and exposes Count +
// Info over gRPC. One Server per process ; safe for concurrent RPCs.
type Server struct {
	counter *tokencount.Counter
	readyAt int64
}

// NewServer constructs a Server. A nil counter gets a fresh default one.
func NewServer(c *tokencount.Counter) *Server {
	if c == nil {
		c = tokencount.New()
	}
	return &Server{counter: c, readyAt: time.Now().UnixNano()}
}

// Count implements Service : counts each text under the request's model and
// returns per-text counts plus their sum.
func (s *Server) Count(ctx context.Context, req *CountRequest) (*CountResponse, error) {
	if s == nil || s.counter == nil {
		return nil, fmt.Errorf("tokenizer: no counter")
	}
	if req == nil {
		return nil, fmt.Errorf("tokenizer: nil request")
	}
	if len(req.Texts) > MaxBatchSize {
		return nil, fmt.Errorf("tokenizer: batch too large (%d > %d)", len(req.Texts), MaxBatchSize)
	}
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	start := time.Now()
	counts := make([]int, len(req.Texts))
	total := 0
	for i, t := range req.Texts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n := s.counter.Count(t, req.Provider, req.Model)
		counts[i] = n
		total += n
	}
	return &CountResponse{
		Counts:    counts,
		Total:     total,
		ElapsedMs: time.Since(start).Milliseconds(),
	}, nil
}

// Info implements Service.
func (s *Server) Info(_ context.Context, _ *InfoRequest) (*InfoResponse, error) {
	if s == nil {
		return nil, fmt.Errorf("tokenizer: nil server")
	}
	return &InfoResponse{ReadyAt: s.readyAt}, nil
}

// Compile-time guard.
var _ Service = (*Server)(nil)
