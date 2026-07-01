package tokenizer_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/runtime/tokencount"
	"github.com/digitornai/digitorn/internal/tokenizer"
	"github.com/digitornai/digitorn/internal/worker"
)

// buildTokenizerWorker compiles cmd/digitorn-worker-tokenizer to a temp path.
func buildTokenizerWorker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "worker-tokenizer")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", exe,
		"github.com/digitornai/digitorn/cmd/digitorn-worker-tokenizer")
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build worker-tokenizer: %v", err)
	}
	return exe
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestLive_TokenizerWorker_CountsOverGRPC is the real proof : compile the
// worker binary, spawn it as a subprocess via the Manager, and count tokens
// through the gRPC client. The total must match a local Counter — i.e. the
// out-of-process path produces the SAME exact counts as in-process tiktoken.
func TestLive_TokenizerWorker_CountsOverGRPC(t *testing.T) {
	if testing.Short() {
		t.Skip("live worker spawn skipped in -short")
	}
	exe := buildTokenizerWorker(t)

	m := worker.NewManager(quiet())
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.Spawn(ctx, worker.Spec{
		Kind:         tokenizer.Kind,
		Binary:       exe,
		Count:        2, // prove the pool works with >1 instance
		StartTimeout: 10 * time.Second,
	}); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	client := tokenizer.NewClient(m)
	texts := []string{
		"The quick brown fox jumps over the lazy dog.",
		"Another message with a few more tokens than the first one here.",
		"",
	}
	got, err := client.CountTotal(context.Background(), texts, "openai", "gpt-4o")
	if err != nil {
		t.Fatalf("CountTotal over gRPC: %v", err)
	}

	ref := tokencount.New()
	want := 0
	for _, txt := range texts {
		want += ref.Count(txt, "openai", "gpt-4o")
	}
	if got != want {
		t.Fatalf("worker total=%d, want local-exact %d", got, want)
	}
	if got == 0 {
		t.Fatal("expected a non-zero token total")
	}
	t.Logf("PROVEN: out-of-process tokenizer worker (pool of 2) counted %d tokens over gRPC, exact match with local tiktoken", got)
}

// TestLive_TokenizerClient_GracefulWhenNoWorker : with no worker manager (or no
// ready instance) the client returns an error — it NEVER panics or blocks. The
// caller (ContextService) then keeps the provider anchor. This is the
// never-crash contract.
func TestLive_TokenizerClient_GracefulWhenNoWorker(t *testing.T) {
	// nil manager.
	if _, err := tokenizer.NewClient(nil).CountTotal(context.Background(), []string{"hi"}, "openai", "gpt-4o"); err == nil {
		t.Error("nil manager must return an error, not nil")
	}
	// empty input is a fast 0 with no RPC even when degraded.
	if n, err := tokenizer.NewClient(nil).CountTotal(context.Background(), nil, "openai", "gpt-4o"); err != nil || n != 0 {
		t.Errorf("empty input must be 0/nil, got %d/%v", n, err)
	}

	// Manager started but NO worker spawned → Pick fails → error, no hang.
	m := worker.NewManager(quiet())
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})
	c := tokenizer.NewClient(m).WithTimeout(2 * time.Second)
	done := make(chan error, 1)
	go func() {
		_, err := c.CountTotal(context.Background(), []string{"hello"}, "openai", "gpt-4o")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected an error when no worker is ready")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("CountTotal blocked with no worker — must fail fast, never hang the caller")
	}
}
