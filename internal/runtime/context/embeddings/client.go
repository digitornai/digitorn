package embeddings

import (
	"context"
)

const EmbeddingDim = 384

type Vector []float32

type EmbeddingClient interface {
	Embed(ctx context.Context, texts []string) ([]Vector, error)
}

func Cosine(a, b Vector) float32 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float32
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (sqrt32(na) * sqrt32(nb))
}

func CosineNormalized(a, b Vector) float32 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot float32
	for i := range a {
		dot += a[i] * b[i]
	}
	return dot
}

func Normalize(v Vector) Vector {
	if len(v) == 0 {
		return v
	}
	var norm float32
	for _, x := range v {
		norm += x * x
	}
	if norm == 0 {
		return v
	}
	inv := 1 / sqrt32(norm)
	for i := range v {
		v[i] *= inv
	}
	return v
}

func sqrt32(x float32) float32 {
	if x <= 0 {
		return 0
	}

	y := x
	z := x / 2

	for i := 0; i < 8 && z != y; i++ {
		y = z
		z = (y + x/y) / 2
	}
	return z
}
