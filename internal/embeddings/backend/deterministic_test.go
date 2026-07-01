package backend_test

import (
	"context"
	"math"
	"testing"

	"github.com/digitornai/digitorn/internal/embeddings/backend"
)

// =====================================================================
// Deterministic backend correctness.
// =====================================================================

func TestDeterministic_DimensionMatches(t *testing.T) {
	b := backend.NewDeterministic(384)
	if b.Dimension() != 384 {
		t.Errorf("dim = %d", b.Dimension())
	}
}

func TestDeterministic_ModelID(t *testing.T) {
	b := backend.NewDeterministic(384)
	if b.Model() != "deterministic-hash-v1" {
		t.Errorf("model = %q", b.Model())
	}
}

func TestDeterministic_DefaultDim(t *testing.T) {
	// Passing 0 should fall back to 384.
	b := backend.NewDeterministic(0)
	if b.Dimension() != 384 {
		t.Errorf("dim = %d, want 384 fallback", b.Dimension())
	}
}

func TestDeterministic_Reproducible(t *testing.T) {
	b := backend.NewDeterministic(384)
	in := []string{"deploy the app", "list categories"}
	out1, _ := b.Embed(context.Background(), in, true)
	out2, _ := b.Embed(context.Background(), in, true)
	for i := range out1 {
		for j := range out1[i] {
			if out1[i][j] != out2[i][j] {
				t.Fatalf("inputs[%d][%d] divergence : %v vs %v",
					i, j, out1[i][j], out2[i][j])
			}
		}
	}
}

func TestDeterministic_NormalizationProducesUnitNorm(t *testing.T) {
	b := backend.NewDeterministic(384)
	out, _ := b.Embed(context.Background(),
		[]string{"some text with several tokens"}, true)
	var sumSq float64
	for _, x := range out[0] {
		sumSq += float64(x) * float64(x)
	}
	norm := math.Sqrt(sumSq)
	if norm < 0.99 || norm > 1.01 {
		t.Errorf("not unit-norm : %f", norm)
	}
}

func TestDeterministic_EmptyInputProducesZeroVector(t *testing.T) {
	b := backend.NewDeterministic(384)
	out, _ := b.Embed(context.Background(), []string{""}, true)
	for _, x := range out[0] {
		if x != 0 {
			t.Errorf("empty input produced non-zero coord %v", x)
			break
		}
	}
}

func TestDeterministic_SameTokensSimilar(t *testing.T) {
	// Two inputs sharing most tokens should have a non-zero
	// cosine similarity (sanity : the backend isn't pure random).
	b := backend.NewDeterministic(384)
	a, _ := b.Embed(context.Background(), []string{"deploy production app"}, true)
	c, _ := b.Embed(context.Background(), []string{"deploy production server"}, true)
	var dot float64
	for i := range a[0] {
		dot += float64(a[0][i]) * float64(c[0][i])
	}
	if dot <= 0 {
		t.Errorf("shared-token cosine should be > 0, got %f", dot)
	}
}

func TestDeterministic_DifferentTokensLessSimilar(t *testing.T) {
	b := backend.NewDeterministic(384)
	a, _ := b.Embed(context.Background(), []string{"deploy production app"}, true)
	c, _ := b.Embed(context.Background(), []string{"xqzlmn vbwrtq pljkfh"}, true)
	var dot float64
	for i := range a[0] {
		dot += float64(a[0][i]) * float64(c[0][i])
	}
	// Unrelated tokens should have very low cosine.
	if math.Abs(dot) > 0.5 {
		t.Errorf("unrelated cosine too high : %f", dot)
	}
}

func TestDeterministic_BatchHandling(t *testing.T) {
	b := backend.NewDeterministic(384)
	in := make([]string, 100)
	for i := range in {
		in[i] = "input" + string(rune('a'+i%26))
	}
	out, err := b.Embed(context.Background(), in, true)
	if err != nil {
		t.Fatalf("batch err: %v", err)
	}
	if len(out) != 100 {
		t.Errorf("out len = %d, want 100", len(out))
	}
}

func TestDeterministic_CloseIsIdempotent(t *testing.T) {
	b := backend.NewDeterministic(384)
	if err := b.Close(); err != nil {
		t.Errorf("close 1: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Errorf("close 2: %v", err)
	}
}
