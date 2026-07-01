package gateway_test

import (
	"context"
	"testing"

	// Registers the "json" gRPC codec the embeddings service negotiates on.
	_ "github.com/digitornai/digitorn/internal/llm"

	"github.com/digitornai/digitorn/internal/embeddings"
	"github.com/digitornai/digitorn/internal/module/gateway"
)

// The gateway round-trips an embed call from a worker-style client
// (NewDirectClient over the gateway conn) to the daemon-side forwarder,
// carrying the model + role and returning the forwarder's vectors.
func TestGateway_EmbedRoundTrip(t *testing.T) {
	const secret = "round-trip-secret"
	var gotModel, gotRole string
	gotInputs := 0
	embed := func(_ context.Context, req *embeddings.EmbedRequest) (*embeddings.EmbedResponse, error) {
		gotModel, gotRole, gotInputs = req.Model, req.Role, len(req.Inputs)
		vecs := make([][]float32, len(req.Inputs))
		for i := range req.Inputs {
			vecs[i] = []float32{1, 2, 3}
		}
		return &embeddings.EmbedResponse{Vectors: vecs, Model: req.Model, Dimension: 3}, nil
	}

	rerank := func(_ context.Context, req *embeddings.RerankRequest) (*embeddings.RerankResponse, error) {
		scores := make([]float32, len(req.Documents))
		for i := range req.Documents {
			scores[i] = float32(len(req.Documents) - i)
		}
		return &embeddings.RerankResponse{Scores: scores, Model: req.Model}, nil
	}

	srv, err := gateway.Start(secret, embed, rerank)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	cc, err := gateway.Dial(srv.Addr(), secret)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cc.Close()

	client := embeddings.NewDirectClient(cc)
	vecs, dim, err := client.EmbedModel(context.Background(), "bge-m3", embeddings.RoleQuery, []string{"a", "b"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if dim != 3 || len(vecs) != 2 || len(vecs[0]) != 3 {
		t.Fatalf("dim=%d vecs=%d width=%d", dim, len(vecs), len(vecs[0]))
	}
	if gotModel != "bge-m3" || gotRole != embeddings.RoleQuery || gotInputs != 2 {
		t.Errorf("forwarder saw model=%q role=%q inputs=%d", gotModel, gotRole, gotInputs)
	}

	scores, err := client.Rerank(context.Background(), "bge-reranker", "q", []string{"d1", "d2", "d3"})
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(scores) != 3 || scores[0] <= scores[2] {
		t.Errorf("rerank scores wrong: %v", scores)
	}
}

// A client presenting the wrong secret is rejected by the gateway auth.
func TestGateway_RejectsBadSecret(t *testing.T) {
	embed := func(_ context.Context, req *embeddings.EmbedRequest) (*embeddings.EmbedResponse, error) {
		return &embeddings.EmbedResponse{Dimension: 3}, nil
	}
	srv, err := gateway.Start("right-secret", embed, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	cc, err := gateway.Dial(srv.Addr(), "wrong-secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cc.Close()

	client := embeddings.NewDirectClient(cc)
	if _, _, err := client.EmbedModel(context.Background(), "", "", []string{"x"}); err == nil {
		t.Error("expected auth error with wrong secret, got nil")
	}
}
