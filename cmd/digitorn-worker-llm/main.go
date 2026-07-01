// digitorn-worker-llm is the subprocess that hosts the LLM provider
// module. It runs Bifrost in-process and exposes the llm.Service over
// gRPC (JSON codec) to the daemon. ENV-driven config :
//
//	DIGITORN_WORKER_SECRET     — shared HMAC secret (set by the manager)
//	DIGITORN_WORKER_KIND       — should be "llm"
//	DIGITORN_LLM_GATEWAY_URL   — digitorn gateway base URL (optional)
//	DIGITORN_LLM_CONCURRENCY   — per-provider concurrency (default 256)
//	DIGITORN_LLM_BUFFER        — per-provider buffer size (default 16384)
//	DIGITORN_LLM_PER_PROVIDER_CONCURRENCY — JSON map, e.g. {"anthropic":1024,"deepseek":64}
//	DIGITORN_LLM_PER_PROVIDER_BUFFER      — JSON map, e.g. {"anthropic":32768,"deepseek":1024}
//	DIGITORN_LLM_CB_THRESHOLD             — failures before CB opens (default 3)
//	DIGITORN_LLM_CB_WINDOW                — rolling failure window, e.g. "30s" (default 30s)
//	DIGITORN_LLM_CB_OPEN_FOR              — cooldown before HALF-OPEN probe, e.g. "5s" (default 5s, prod recommend 30s)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"google.golang.org/grpc"

	"github.com/digitornai/digitorn/internal/llm"
	llmbifrost "github.com/digitornai/digitorn/internal/llm/bifrost"
	"github.com/digitornai/digitorn/internal/worker"
)

func main() {
	hs, err := worker.ReadEnvHandshake()
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker-llm: bad handshake: %v\n", err)
		os.Exit(2)
	}

	cfg := llmbifrost.Config{
		GatewayURL:             os.Getenv("DIGITORN_LLM_GATEWAY_URL"),
		Concurrency:            envInt("DIGITORN_LLM_CONCURRENCY", 256),
		BufferSize:             envInt("DIGITORN_LLM_BUFFER", 16384),
		PerProviderConcurrency: envIntMap("DIGITORN_LLM_PER_PROVIDER_CONCURRENCY"),
		PerProviderBufferSize:  envIntMap("DIGITORN_LLM_PER_PROVIDER_BUFFER"),
		// Circuit breaker tuning — env-overridable, 0 keeps the defaults
		// from NewCircuitBreakerPlugin (3 / 30s / 5s).
		CBThreshold: envInt("DIGITORN_LLM_CB_THRESHOLD", 0),
		CBWindow:    envDur("DIGITORN_LLM_CB_WINDOW", 0),
		CBOpenFor:   envDur("DIGITORN_LLM_CB_OPEN_FOR", 0),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc, err := llmbifrost.NewService(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker-llm: bifrost init: %v\n", err)
		os.Exit(3)
	}
	defer svc.Shutdown()

	bindAddr := os.Getenv("DIGITORN_WORKER_BIND")
	if bindAddr == "" {
		bindAddr = "127.0.0.1:0"
	}

	if err := worker.Run(worker.ServerConfig{
		Handshake: hs,
		BindAddr:  bindAddr,
		Register: func(s *grpc.Server) {
			llm.RegisterService(s, svc)
		},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "worker-llm: run: %v\n", err)
		os.Exit(1)
	}
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// envDur parses a Go duration ("5s", "1m30s", "500ms") from an env var.
// Empty / unparseable → def (typically 0 = "let the constructor pick").
func envDur(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		fmt.Fprintf(os.Stderr, "worker-llm: ignoring malformed %s=%q: %v\n", key, v, err)
		return def
	}
	return d
}

// envIntMap parses a JSON object of provider→int from an env var.
// Empty / unparseable → nil (account.go falls back to global defaults).
func envIntMap(key string) map[string]int {
	v := os.Getenv(key)
	if v == "" {
		return nil
	}
	out := map[string]int{}
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		fmt.Fprintf(os.Stderr, "worker-llm: ignoring malformed %s: %v\n", key, err)
		return nil
	}
	return out
}
