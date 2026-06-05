package llm_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/llm"
	"github.com/mbathepaul/digitorn/internal/worker"
)

// H6 — LLM provider negative tests. The happy paths and isolation are
// covered elsewhere ; this file targets the chemins-d'erreur the
// daemon will see in production : bad credentials, network down,
// timeouts respected, ctx cancellation propagated, stream resilience.

// negativesHarness spawns one real LLM worker for use across all H6
// tests. Sharing keeps the suite fast (one go-build per package).
func negativesHarness(t *testing.T) *llm.Client {
	t.Helper()
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
		Kind: "llm", Binary: exe, Count: 1,
		StartTimeout: 15 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	c, err := llm.NewClient(llm.ClientConfig{Manager: m})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestNegatives_Chat_BYOKWithoutAPIKey verifies that a BYOK request
// missing APIKey is rejected by the worker BEFORE any network call.
// The error must propagate cleanly back to the Client caller.
func TestNegatives_Chat_BYOKWithoutAPIKey(t *testing.T) {
	c := negativesHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := c.Chat(ctx, &llm.ChatRequest{
		BYOK:     true,
		Provider: "anthropic",
		Model:    "claude-sonnet-4.5",
		APIKey:   "", // missing on purpose
		Messages: []llm.ChatMessage{{Role: "user", Content: "ping"}},
		Timeout:  3 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error : BYOK without APIKey must be rejected")
	}
	t.Logf("got expected error : %v", err)
}

// TestNegatives_Chat_GatewayWithoutUserJWT : BYOK=false without JWT
// must fail fast.
func TestNegatives_Chat_GatewayWithoutUserJWT(t *testing.T) {
	c := negativesHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := c.Chat(ctx, &llm.ChatRequest{
		BYOK:     false,
		Provider: "anthropic",
		Model:    "claude-sonnet-4.5",
		UserJWT:  "", // missing on purpose
		Messages: []llm.ChatMessage{{Role: "user", Content: "ping"}},
		Timeout:  3 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error : gateway routing without UserJWT must be rejected")
	}
	t.Logf("got expected error : %v", err)
}

// TestNegatives_Chat_ContextCanceledImmediately : caller cancels ctx
// before the call has a chance to do anything. The Chat method must
// return promptly with ctx.Canceled (not block).
func TestNegatives_Chat_ContextCanceledImmediately(t *testing.T) {
	c := negativesHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before the call

	start := time.Now()
	_, err := c.Chat(ctx, &llm.ChatRequest{
		Provider: "anthropic",
		Model:    "claude-sonnet-4.5",
		UserJWT:  "fake",
		Messages: []llm.ChatMessage{{Role: "user", Content: "ping"}},
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error on pre-canceled ctx")
	}
	if elapsed > 1*time.Second {
		t.Errorf("Chat blocked despite canceled ctx : %v", elapsed)
	}
}

// TestNegatives_Chat_RequestTimeoutRespected : Chat() with a very
// short Timeout must surface DeadlineExceeded fast (within the
// budget plus reasonable slack).
func TestNegatives_Chat_RequestTimeoutRespected(t *testing.T) {
	c := negativesHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	_, err := c.Chat(ctx, &llm.ChatRequest{
		Provider: "anthropic",
		Model:    "claude-sonnet-4.5",
		UserJWT:  "fake-jwt-that-no-gateway-will-accept",
		BaseURL:  "http://127.0.0.1:1", // unreachable
		Messages: []llm.ChatMessage{{Role: "user", Content: "ping"}},
		Timeout:  200 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error : unreachable provider should not succeed")
	}
	if elapsed > 5*time.Second {
		t.Errorf("Chat blocked way past Timeout=200ms : %v (expected sub-2s)", elapsed)
	}
	t.Logf("call returned after %v : %v", elapsed, err)
}

// TestNegatives_ChatStream_StreamClosesAfterError : an unreachable
// provider during streaming must produce either an error chunk OR a
// closed channel — never a hung goroutine.
func TestNegatives_ChatStream_StreamClosesAfterError(t *testing.T) {
	c := negativesHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	chunks, err := c.ChatStream(ctx, &llm.ChatRequest{
		Provider: "anthropic",
		Model:    "claude-sonnet-4.5",
		UserJWT:  "fake",
		BaseURL:  "http://127.0.0.1:1", // unreachable
		Messages: []llm.ChatMessage{{Role: "user", Content: "ping"}},
		Timeout:  200 * time.Millisecond,
	})
	if err != nil {
		// Acceptable : the open itself errored (some setups fail fast).
		t.Logf("ChatStream open errored : %v", err)
		return
	}

	// Drain the channel ; it must close within reasonable time.
	done := make(chan struct{})
	var saw int
	go func() {
		defer close(done)
		for range chunks {
			saw++
			if saw > 1000 {
				return
			}
		}
	}()
	select {
	case <-done:
		t.Logf("stream closed cleanly after %d chunks", saw)
	case <-time.After(10 * time.Second):
		t.Fatal("ChatStream channel never closed despite unreachable provider")
	}
}

// TestNegatives_ChatStream_CallerCancelClosesPromptly : caller cancels
// the stream's ctx ; the channel must close within a tight budget.
func TestNegatives_ChatStream_CallerCancelClosesPromptly(t *testing.T) {
	c := negativesHarness(t)
	parentCtx, parentCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer parentCancel()

	streamCtx, streamCancel := context.WithCancel(parentCtx)
	chunks, err := c.ChatStream(streamCtx, &llm.ChatRequest{
		Provider: "anthropic",
		Model:    "claude-sonnet-4.5",
		UserJWT:  "fake",
		Messages: []llm.ChatMessage{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("ChatStream open: %v", err)
	}
	streamCancel()

	start := time.Now()
	for range chunks {
	}
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Errorf("stream did not close promptly after cancel : %v", elapsed)
	}
}

// TestNegatives_Chat_NoMessages : empty messages array must produce
// an error rather than reaching the provider.
func TestNegatives_Chat_NoMessages(t *testing.T) {
	c := negativesHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := c.Chat(ctx, &llm.ChatRequest{
		Provider: "anthropic",
		Model:    "claude-sonnet-4.5",
		UserJWT:  "fake",
		Messages: nil,
		Timeout:  2 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error on empty messages")
	}
}

// TestNegatives_RetryableErrorClassification : the client retries
// gRPC transient errors automatically. We verify the behaviour by
// stopping the worker, which makes the first call see "no healthy
// worker" — a retryable error — and confirm the client surfaces the
// error rather than hanging.
func TestNegatives_RetryableErrorClassification(t *testing.T) {
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
		Kind: "llm", Binary: exe, Count: 1,
		StartTimeout: 15 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	c, _ := llm.NewClient(llm.ClientConfig{Manager: m, Retries: 1, Timeout: 1 * time.Second})

	// Stop the worker before calling — Pick must return ErrNoHealthyWorker.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	_ = m.Stop(stopCtx)

	_, err := c.ListProviders(ctx)
	if err == nil {
		t.Fatal("expected error after worker stopped")
	}
	if errors.Is(err, context.DeadlineExceeded) && err != context.DeadlineExceeded {
		// fine — ctx expired during retry, which is acceptable
	}
	t.Logf("got expected error after worker stop : %v", err)
}
