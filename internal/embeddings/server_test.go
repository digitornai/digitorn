package embeddings_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/digitornai/digitorn/internal/embeddings"
)

// =====================================================================
// CE-8 — Server (Service) tests, deterministic backend.
// =====================================================================

func newSrv() *embeddings.Server {
	mgr := embeddings.NewManager("", embeddings.ModeDeterministic, false, nil)
	return embeddings.NewServer(mgr)
}

// 1. Info returns model + dim
func TestServer_Info(t *testing.T) {
	srv := newSrv()
	out, err := srv.Info(context.Background(), &embeddings.InfoRequest{})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if out.Dimension != embeddings.EmbeddingDim {
		t.Errorf("dim = %d, want %d", out.Dimension, embeddings.EmbeddingDim)
	}
	if out.Model == "" {
		t.Error("model empty")
	}
}

// 2. Empty inputs returns empty vectors, no error
func TestServer_EmptyInputs(t *testing.T) {
	srv := newSrv()
	out, err := srv.Embed(context.Background(), &embeddings.EmbedRequest{Inputs: nil})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(out.Vectors) != 0 {
		t.Errorf("want 0 vectors, got %d", len(out.Vectors))
	}
	if out.Dimension != embeddings.EmbeddingDim {
		t.Errorf("dim = %d", out.Dimension)
	}
}

// 3. Single text → one vector of EmbeddingDim
func TestServer_SingleText(t *testing.T) {
	srv := newSrv()
	out, err := srv.Embed(context.Background(), &embeddings.EmbedRequest{
		Inputs:    []string{"lis-moi un fichier"},
		Normalize: true,
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(out.Vectors) != 1 {
		t.Fatalf("vectors = %d, want 1", len(out.Vectors))
	}
	if len(out.Vectors[0]) != embeddings.EmbeddingDim {
		t.Errorf("dim = %d, want %d", len(out.Vectors[0]), embeddings.EmbeddingDim)
	}
}

// 4. Reproducibility : same input twice → same vector
func TestServer_Reproducible(t *testing.T) {
	srv := newSrv()
	out1, _ := srv.Embed(context.Background(), &embeddings.EmbedRequest{
		Inputs: []string{"deploy the app"}, Normalize: true,
	})
	out2, _ := srv.Embed(context.Background(), &embeddings.EmbedRequest{
		Inputs: []string{"deploy the app"}, Normalize: true,
	})
	for i, v := range out1.Vectors[0] {
		if v != out2.Vectors[0][i] {
			t.Fatalf("vec[%d] diverges : %v vs %v", i, v, out2.Vectors[0][i])
		}
	}
}

// 5. L2 normalisation
func TestServer_L2Normalized(t *testing.T) {
	srv := newSrv()
	out, _ := srv.Embed(context.Background(), &embeddings.EmbedRequest{
		Inputs: []string{"hello world this is a test"}, Normalize: true,
	})
	var sumSq float64
	for _, x := range out.Vectors[0] {
		sumSq += float64(x) * float64(x)
	}
	// Squared norm should be 1.0 ± epsilon when normalize=true.
	if sumSq < 0.99 || sumSq > 1.01 {
		t.Errorf("not L2-normalised : ||v||² = %f", sumSq)
	}
}

// 6. Batch larger than MaxBatchSize rejected
func TestServer_RejectsOverlargeBatch(t *testing.T) {
	srv := newSrv()
	big := make([]string, embeddings.MaxBatchSize+1)
	for i := range big {
		big[i] = "x"
	}
	_, err := srv.Embed(context.Background(), &embeddings.EmbedRequest{Inputs: big})
	if err == nil {
		t.Error("expected error for batch > MaxBatchSize")
	}
}

// 7. Concurrent Embed calls safe
func TestServer_ConcurrentSafe(t *testing.T) {
	srv := newSrv()
	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			_, err := srv.Embed(context.Background(), &embeddings.EmbedRequest{
				Inputs:    []string{"text" + string(rune('a'+i%26))},
				Normalize: true,
			})
			if err != nil {
				t.Errorf("[%d] embed: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
}

// 8. Different inputs → different vectors
func TestServer_DiscriminatesInputs(t *testing.T) {
	srv := newSrv()
	out, _ := srv.Embed(context.Background(), &embeddings.EmbedRequest{
		Inputs:    []string{"alpha beta gamma", "totally unrelated tokens"},
		Normalize: true,
	})
	if vecsEqual(out.Vectors[0], out.Vectors[1]) {
		t.Error("unrelated inputs produced identical vectors")
	}
}

func vecsEqual(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// 9. Long inputs don't crash
func TestServer_LongInputDoesNotCrash(t *testing.T) {
	srv := newSrv()
	long := strings.Repeat("token ", 1000)
	_, err := srv.Embed(context.Background(), &embeddings.EmbedRequest{
		Inputs: []string{long}, Normalize: true,
	})
	if err != nil {
		t.Fatalf("long input err: %v", err)
	}
}
