package embeddings

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
)

type MockClient struct{}

func (MockClient) Embed(_ context.Context, texts []string) ([]Vector, error) {
	out := make([]Vector, len(texts))
	for i, t := range texts {
		out[i] = mockEmbed(t)
	}
	return out, nil
}

func mockEmbed(text string) Vector {
	tokens := strings.Fields(strings.ToLower(text))
	v := make(Vector, EmbeddingDim)
	for _, tok := range tokens {
		h := fnv.New64a()
		_, _ = h.Write([]byte(tok))
		seed := h.Sum64()
		for i := 0; i < EmbeddingDim; i++ {
			phase := float64(seed) + float64(i)*0.137
			v[i] += float32(math.Sin(phase))
		}
	}
	return Normalize(v)
}
