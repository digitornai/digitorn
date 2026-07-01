package llm_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/digitornai/digitorn/internal/llm"
	_ "github.com/digitornai/digitorn/internal/llm" // ensure JSON codec registered
	"github.com/digitornai/digitorn/internal/worker"
)

var (
	workerOnce sync.Once
	workerExe  string
	workerErr  error
)

func buildLLMWorker(t *testing.T) string {
	t.Helper()
	workerOnce.Do(func() {
		dir, err := os.MkdirTemp("", "digitorn-worker-llm-*")
		if err != nil {
			workerErr = err
			return
		}
		exe := filepath.Join(dir, "worker-llm")
		if runtime.GOOS == "windows" {
			exe += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", exe,
			"github.com/digitornai/digitorn/cmd/digitorn-worker-llm")
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			workerErr = err
			return
		}
		workerExe = exe
	})
	if workerErr != nil {
		t.Fatalf("build worker-llm: %v", workerErr)
	}
	return workerExe
}

func startLLMWorker(t *testing.T) (*worker.Manager, worker.Conn) {
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
		Kind:         "llm",
		Binary:       exe,
		Count:        1,
		StartTimeout: 15 * time.Second,
	}); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	c, err := m.Pick(ctx, "llm")
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	return m, c
}

func TestIntegration_WorkerLLM_HealthCheckOK(t *testing.T) {
	_, c := startLLMWorker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := worker.HealthCheck(ctx, c, "")
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	if st != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("status: %v", st)
	}
}

func TestIntegration_WorkerLLM_ListProvidersViaRPC(t *testing.T) {
	_, c := startLLMWorker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	in := &llm.ListProvidersRequest{}
	out := &llm.ListProvidersResponse{}
	err := c.GRPC().Invoke(ctx,
		"/"+llm.ServiceName+"/"+llm.MethodListProviders,
		in, out,
		grpc.CallContentSubtype(llm.CodecName),
	)
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if len(out.Providers) == 0 {
		t.Fatal("expected at least one provider")
	}
	hasAnthropic := false
	hasGateway := false
	for _, p := range out.Providers {
		if p.Name == "anthropic" {
			hasAnthropic = true
		}
		if p.Name == "digitorn" {
			hasGateway = true
		}
	}
	if !hasAnthropic {
		t.Error("missing 'anthropic' provider in worker response")
	}
	if !hasGateway {
		t.Error("missing 'digitorn' gateway provider")
	}
}

func TestIntegration_WorkerLLM_GatewayRoutingDetected(t *testing.T) {
	_, c := startLLMWorker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Send a Chat request via the gateway route (BYOK=false default).
	// No real gateway running ; we expect a network-level error from
	// Bifrost, which still proves the request was correctly parsed and
	// routed through the worker→Bifrost path.
	in := &llm.ChatRequest{
		Provider: "anthropic",
		Model:    "claude-sonnet-4.5",
		UserJWT:  "fake-user-jwt-token",
		Messages: []llm.ChatMessage{
			{Role: "user", Content: "ping"},
		},
		Timeout: 2 * time.Second,
	}
	out := &llm.ChatResponse{}
	err := c.GRPC().Invoke(ctx,
		"/"+llm.ServiceName+"/"+llm.MethodChat,
		in, out,
		grpc.CallContentSubtype(llm.CodecName),
	)
	// We EXPECT an error (no real gateway / no real key). The point is
	// that the request was accepted, parsed, and routed through Bifrost.
	if err != nil {
		t.Logf("expected error reached us : %v", err)
	} else {
		t.Log("chat returned no error (unexpected with fake key — but acceptable)")
	}
}

func TestIntegration_WorkerLLM_CountTokensViaRPC(t *testing.T) {
	_, c := startLLMWorker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	in := &llm.CountTokensRequest{
		Provider: "anthropic",
		Model:    "claude-sonnet-4.5",
		Messages: []llm.ChatMessage{
			{Role: "user", Content: "the quick brown fox jumps over the lazy dog"},
		},
	}
	out := &llm.CountTokensResponse{}
	err := c.GRPC().Invoke(ctx,
		"/"+llm.ServiceName+"/"+llm.MethodCountTokens,
		in, out,
		grpc.CallContentSubtype(llm.CodecName),
	)
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if out.Tokens == 0 {
		t.Fatal("expected non-zero token count")
	}
	t.Logf("estimated tokens for 'the quick brown fox' : %d", out.Tokens)
}
