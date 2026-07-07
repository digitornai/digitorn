package embeddings

import (
	"context"
	"fmt"
	"time"
)


type Router interface {

	Embed(ctx context.Context, model, role string, inputs []string, normalize bool) ([][]float32, string, int, error)

	Rerank(ctx context.Context, model, query string, docs []string) ([]float32, string, error)

	DefaultModel(ctx context.Context) (string, int, error)
}


type Server struct {
	router  Router
	readyAt int64
}


func NewServer(r Router) *Server {
	return &Server{router: r, readyAt: time.Now().UnixNano()}
}


func (s *Server) Embed(ctx context.Context, req *EmbedRequest) (*EmbedResponse, error) {
	if s == nil || s.router == nil {
		return nil, fmt.Errorf("embeddings: no router")
	}
	if req == nil {
		return nil, fmt.Errorf("embeddings: nil request")
	}
	if len(req.Inputs) > MaxBatchSize {
		return nil, fmt.Errorf("embeddings: batch too large (%d > %d)", len(req.Inputs), MaxBatchSize)
	}
	if len(req.Inputs) == 0 {
		model, dim, err := s.router.DefaultModel(ctx)
		if req.Model != "" {
			// Echo the requested model's identity even for an empty batch.
			if vs, m, d, e := s.router.Embed(ctx, req.Model, req.Role, nil, req.Normalize); e == nil {
				_ = vs
				model, dim, err = m, d, nil
			} else {
				err = e
			}
		}
		if err != nil {
			return nil, err
		}
		return &EmbedResponse{Vectors: nil, Model: model, Dimension: dim}, nil
	}

	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	start := time.Now()
	vecs, model, dim, err := s.router.Embed(ctx, req.Model, req.Role, req.Inputs, req.Normalize)
	if err != nil {
		return nil, fmt.Errorf("embeddings: backend: %w", err)
	}

	for i, v := range vecs {
		if len(v) != dim {
			return nil, fmt.Errorf("embeddings: backend returned vec[%d] dim=%d, want %d",
				i, len(v), dim)
		}
	}
	return &EmbedResponse{
		Vectors:   vecs,
		Model:     model,
		Dimension: dim,
		ElapsedMs: time.Since(start).Milliseconds(),
	}, nil
}


func (s *Server) Rerank(ctx context.Context, req *RerankRequest) (*RerankResponse, error) {
	if s == nil || s.router == nil {
		return nil, fmt.Errorf("embeddings: no router")
	}
	if req == nil || len(req.Documents) == 0 {
		return &RerankResponse{}, nil
	}
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	scores, model, err := s.router.Rerank(ctx, req.Model, req.Query, req.Documents)
	if err != nil {
		return nil, fmt.Errorf("embeddings: rerank: %w", err)
	}
	return &RerankResponse{Scores: scores, Model: model}, nil
}


func (s *Server) Info(ctx context.Context, _ *InfoRequest) (*InfoResponse, error) {
	if s == nil || s.router == nil {
		return nil, fmt.Errorf("embeddings: no router")
	}
	model, dim, err := s.router.DefaultModel(ctx)
	if err != nil {
		return nil, err
	}
	return &InfoResponse{
		Model:     model,
		Dimension: dim,
		ReadyAt:   s.readyAt,
	}, nil
}


var _ Service = (*Server)(nil)
