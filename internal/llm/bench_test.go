package llm_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/worker"
)

// L7 — benchmarks. We measure the daemon → worker → Bifrost overhead
// *without* hitting an external provider, so the numbers reflect ONLY
// our infrastructure (gRPC + JSON codec + worker dispatch). Calls that
// would normally go to api.openai.com / api.anthropic.com are avoided
// by using CountTokens (local estimate) and ListProviders (in-memory
// table).

func benchSetup(b *testing.B, count int) (*llm.Client, *worker.Manager) {
	b.Helper()
	exe := buildLLMWorkerB(b)
	m := worker.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	_ = m.Start()
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, worker.Spec{
		Kind:         "llm",
		Binary:       exe,
		Count:        count,
		StartTimeout: 15 * time.Second,
	}); err != nil {
		b.Fatal(err)
	}
	client, _ := llm.NewClient(llm.ClientConfig{Manager: m})
	return client, m
}

func BenchmarkLLM_ListProviders_OverheadDaemonToWorker(b *testing.B) {
	client, _ := benchSetup(b, 1)
	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := client.ListProviders(ctx)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLLM_CountTokens_OverheadDaemonToWorker(b *testing.B) {
	client, _ := benchSetup(b, 1)
	ctx := context.Background()
	req := &llm.CountTokensRequest{
		Provider: "anthropic", Model: "x",
		Messages: []llm.ChatMessage{{Role: "user", Content: "the quick brown fox"}},
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := client.CountTokens(ctx, req)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLLM_CountTokens_Parallel(b *testing.B) {
	client, _ := benchSetup(b, 4)
	ctx := context.Background()
	req := &llm.CountTokensRequest{
		Provider: "anthropic", Model: "x",
		Messages: []llm.ChatMessage{{Role: "user", Content: "the quick brown fox"}},
	}
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := client.CountTokens(ctx, req)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

// buildLLMWorkerB is the *testing.B equivalent of buildLLMWorker.
func buildLLMWorkerB(b *testing.B) string {
	b.Helper()
	// Reuse the same once-built binary as the test suite to avoid 4s rebuilds.
	t := &testing.T{}
	return buildLLMWorker(t)
}
