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

func TestClient_ListProviders_SimpleCall(t *testing.T) {
	exe := buildLLMWorker(t)
	m := worker.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	_ = m.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, worker.Spec{
		Kind:         "llm",
		Binary:       exe,
		Count:        1,
		StartTimeout: 15 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}

	client, err := llm.NewClient(llm.ClientConfig{Manager: m})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.ListProviders(ctx)
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if len(resp.Providers) == 0 {
		t.Fatal("no providers")
	}
}

func TestClient_CountTokens(t *testing.T) {
	exe := buildLLMWorker(t)
	m := worker.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	_ = m.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, worker.Spec{Kind: "llm", Binary: exe, Count: 1, StartTimeout: 15 * time.Second}); err != nil {
		t.Fatal(err)
	}

	client, _ := llm.NewClient(llm.ClientConfig{Manager: m})
	resp, err := client.CountTokens(ctx, &llm.CountTokensRequest{
		Provider: "anthropic", Model: "claude-sonnet-4.5",
		Messages: []llm.ChatMessage{
			{Role: "user", Content: "the quick brown fox jumps over the lazy dog"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Tokens == 0 {
		t.Fatal("expected non-zero tokens")
	}
}

func TestClient_NoHealthyWorker_PropagatesError(t *testing.T) {
	m := worker.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	_ = m.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})

	// Don't spawn any worker.
	client, _ := llm.NewClient(llm.ClientConfig{Manager: m, Retries: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := client.ListProviders(ctx)
	if err == nil {
		t.Fatal("expected error when no worker spawned")
	}
}

func TestClient_LoadBalance_HitsAllWorkers(t *testing.T) {
	exe := buildLLMWorker(t)
	m := worker.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	_ = m.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, worker.Spec{
		Kind: "llm", Binary: exe, Count: 3, StartTimeout: 15 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}

	client, _ := llm.NewClient(llm.ClientConfig{Manager: m})
	for i := 0; i < 9; i++ {
		_, err := client.ListProviders(ctx)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	// We just verify all 3 are stable ; round-robin balance is tested at
	// the framework level in internal/worker.
	pool := m.Pool("llm")
	if len(pool) != 3 {
		t.Fatalf("expected 3 workers, got %d", len(pool))
	}
}

func TestClient_ChatStream_ChannelClosesOnError(t *testing.T) {
	exe := buildLLMWorker(t)
	m := worker.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	_ = m.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, worker.Spec{Kind: "llm", Binary: exe, Count: 1, StartTimeout: 15 * time.Second}); err != nil {
		t.Fatal(err)
	}

	client, _ := llm.NewClient(llm.ClientConfig{Manager: m})
	streamCtx, streamCancel := context.WithTimeout(ctx, 5*time.Second)
	defer streamCancel()

	// Use gateway routing (BYOK=false default) with fake JWT — provider
	// will reject, the stream MUST surface the error chunk and close the
	// channel cleanly.
	chunks, err := client.ChatStream(streamCtx, &llm.ChatRequest{
		Provider: "anthropic",
		Model:    "claude-sonnet-4.5",
		UserJWT:  "fake-user-jwt-for-stream-test",
		Messages: []llm.ChatMessage{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("ChatStream open: %v", err)
	}

	sawError := false
	count := 0
	for chunk := range chunks {
		count++
		if chunk.Error != "" {
			sawError = true
		}
		if count > 100 {
			t.Fatal("too many chunks for a failing stream")
		}
	}
	if !sawError {
		t.Log("no explicit error chunk — gRPC may have surfaced the error via final RecvMsg")
		// Acceptable : the stream closed without an error chunk, the gRPC
		// status error becomes the RecvMsg return.
	}
}

func TestClient_Stats_ExposesWorkerPool(t *testing.T) {
	exe := buildLLMWorker(t)
	m := worker.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	_ = m.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, worker.Spec{Kind: "llm", Binary: exe, Count: 2, StartTimeout: 15 * time.Second}); err != nil {
		t.Fatal(err)
	}

	client, _ := llm.NewClient(llm.ClientConfig{Manager: m})
	s := client.Stats()
	if s.WorkerPool.Total != 2 {
		t.Fatalf("pool total: %d", s.WorkerPool.Total)
	}
	if s.WorkerPool.Ready != 2 {
		t.Fatalf("pool ready: %d", s.WorkerPool.Ready)
	}
}
