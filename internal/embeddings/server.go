package embeddings

import (
	"context"
	"fmt"
	"time"

	"github.com/mbathepaul/digitorn/internal/embeddings/backend"
)

// Server is the production Service implementation : it wraps an
// inference backend and exposes Embed + Info over gRPC. Used by
// the worker binary main(). One Server instance per process ;
// the worker.Run loop spawns goroutines.
type Server struct {
	be      backend.Backend
	readyAt int64
}

// NewServer constructs a Server bound to the given backend.
func NewServer(be backend.Backend) *Server {
	return &Server{be: be, readyAt: time.Now().UnixNano()}
}

// Embed implements Service. Validates the request, delegates to
// the backend, returns the doc-conform shape.
func (s *Server) Embed(ctx context.Context, req *EmbedRequest) (*EmbedResponse, error) {
	if s == nil || s.be == nil {
		return nil, fmt.Errorf("embeddings: no backend")
	}
	if req == nil {
		return nil, fmt.Errorf("embeddings: nil request")
	}
	if len(req.Inputs) > MaxBatchSize {
		return nil, fmt.Errorf("embeddings: batch too large (%d > %d)", len(req.Inputs), MaxBatchSize)
	}
	if len(req.Inputs) == 0 {
		return &EmbedResponse{
			Vectors:   nil,
			Model:     s.be.Model(),
			Dimension: s.be.Dimension(),
		}, nil
	}
	// Honour per-call timeout when set.
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	start := time.Now()
	vecs, err := s.be.Embed(ctx, req.Inputs, req.Normalize)
	if err != nil {
		return nil, fmt.Errorf("embeddings: backend: %w", err)
	}
	dim := s.be.Dimension()
	// Defensive : dimension consistency check before going on the wire.
	for i, v := range vecs {
		if len(v) != dim {
			return nil, fmt.Errorf("embeddings: backend returned vec[%d] dim=%d, want %d",
				i, len(v), dim)
		}
	}
	return &EmbedResponse{
		Vectors:   vecs,
		Model:     s.be.Model(),
		Dimension: dim,
		ElapsedMs: time.Since(start).Milliseconds(),
	}, nil
}

// Info implements Service.
func (s *Server) Info(_ context.Context, _ *InfoRequest) (*InfoResponse, error) {
	if s == nil || s.be == nil {
		return nil, fmt.Errorf("embeddings: no backend")
	}
	return &InfoResponse{
		Model:     s.be.Model(),
		Dimension: s.be.Dimension(),
		ReadyAt:   s.readyAt,
	}, nil
}

// Compile-time guard.
var _ Service = (*Server)(nil)
